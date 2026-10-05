// The handoff surfaces' rules as tests (team rest-of-release plan §6.1–§6.3, §6.5,
// §8.2, §14 Q14–Q23, Q31; criteria 62, 68, 77, 78, 84, 87, 92, 94). Fixtures are shaped
// like the Go response types: teamHandoffItem, teamHandoffOpen, teamHandoffSendResponse,
// teamHandoffOpenRuntime and teamHandoffRecall.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { withSide, stateView, stateLine, receiptLine, railMeta, railGroup, endedRowWords, itemTitle, itemActions, neverArrived, openView, readinessView, firstReady,
  windowWords, railWhen, continuesLine, contextRowName, recipientChoices, noRecipientWords, draftForm, formRequest, formKey, formProblem, willBeSent, sheetSentences,
  sendRefusal, agentRows, adoptedProfileIds, documentFacts, recallView, recallConsent, recallOutcome, relatedHandoffRows, nativeIdsOf,
  shortSession, bytesWords } from './handoff-model.js';

const PRIYA = { user_id: 'usr_priya', display_name: 'Priya Natarajan' };
const DANIEL = { user_id: 'usr_daniel', display_name: 'Daniel Reyes' };
const labels = runtime => ({ alpha: 'Alpha', beta: 'Beta' })[runtime] || runtime;

function item(extra = {}) {
  return { id: 'hnd_1', organization_id: 'org_1', sent_here: false, to_me: true, peer: DANIEL, title: 'Rate limiter: rules 4–6 remain',
    state: 'received', created_at: '2026-10-04T10:12:00Z', state_at: '2026-10-04T10:13:00Z', has_text: true, ever_held: true,
    offers: ['declined'], open_offered: true, opens: [], side: 'received', ...extra };
}
const sent = (extra = {}) => item({ sent_here: true, to_me: false, peer: PRIYA, side: 'sent', offers: ['withdrawn'], open_offered: false,
  open_withheld_code: 'not_offered', session: { id: 'ses_w', runtime: 'alpha', native_id: 'n-1', catalog_id: 'c-1', resume_id: 'r-1' }, ...extra });
const claimed = (extra = {}) => ({ ticket_id: 'hot_1', runtime: 'beta', state: 'claimed', checkout_root: '/work/limiter',
  session: { runtime: 'beta', native_id: '019a4c00-1111-2222-3333-4444555566e1' }, brief: 'waiting_for_next_prompt', offers: ['deliver-again'], ...extra });

test('the sender’s state words are the eleven of Q17, and a refusal is said in words', () => {
  const words = state => stateView(sent({ state })).word;
  assert.deepEqual(['queued', 'sent', 'received', 'started', 'opened', 'closed', 'declined', 'withdrawn', 'expired', 'refused'].map(words),
    ['queued', 'sent', 'received', 'started', 'opened', 'closed', 'declined', 'withdrawn', 'expired', 'not sent']);
  // "held": queued while the team server carries no handoffs.
  const transport = { server_carries_handoffs: false };
  assert.equal(stateView(sent({ state: 'queued' }), transport).word, 'held');
  assert.equal(stateLine(sent({ state: 'queued' }), { transport }), 'this team server does not carry handoffs');
  assert.equal(stateLine(sent({ state: 'queued' }), { transport: { server_carries_handoffs: true } }), 'waiting to leave this device');
  const refused = code => stateLine(sent({ state: 'refused', refusal_code: code }));
  assert.equal(refused('recipient_inactive'), 'Priya Natarajan is no longer a member of this team');
  assert.equal(refused('unknown_recipient'), 'Priya Natarajan is not a member of this team');
  assert.equal(refused('recipient_inbox_full'), 'Priya Natarajan has too many unopened handoffs');
  assert.equal(refused('rate_limited'), 'too many handoffs were sent this hour');
  assert.equal(refused('over_cap'), 'it is larger than the team server accepts');
  for (const code of ['recipient_inactive', 'rate_limited', 'over_cap', 'conflict']) assert.ok(!refused(code).includes(code), code + ' is shown in words');
});

test('a sent item says who continued it and in which runtime, by the runtime’s published name', () => {
  const opened = sent({ state: 'opened', opened_by: { device_id: 'dev_2', session: { runtime: 'beta', native_id: 'x' } } });
  assert.equal(stateLine(opened, { runtimeLabel: labels }), 'continued by Priya Natarajan in Beta');
  assert.equal(stateLine(sent({ state: 'started', opened_by: opened.opened_by }), { runtimeLabel: labels }), 'Priya Natarajan started a Beta session');
  assert.equal(stateLine(sent({ state: 'declined' })), 'declined by Priya Natarajan');
  assert.equal(stateLine(sent({ state: 'withdrawn' })), 'you withdrew it');
});

test('what a sent item offers: Withdraw while it has not ended; Send again on declined, expired and refused', () => {
  assert.deepEqual(itemActions(sent()).map(action => action.id), ['withdraw']);
  for (const state of ['declined', 'expired', 'refused']) {
    assert.deepEqual(itemActions(sent({ state, offers: [] })).map(action => action.label), ['Send again…'], state);
  }
  assert.deepEqual(itemActions(sent({ state: 'closed', offers: [] })), []);
  assert.deepEqual(itemActions(sent({ state: 'withdrawn', offers: [] })), []);
  // Sent from another of the person's devices: no title, no text held; Withdraw only.
  const other = sent({ sent_here: false, from_another_device: true, title: '', has_text: false, state: 'expired', offers: [] });
  assert.equal(itemTitle(other), 'Sent from another of your devices');
  assert.deepEqual(itemActions(other), [], 'no text here, so nothing to send again');
  assert.deepEqual(itemActions({ ...other, state: 'received', offers: ['withdrawn'] }).map(action => action.id), ['withdraw']);
  const facts = documentFacts(other, null);
  assert.deepEqual(facts.find(row => row.label === 'Text'), { label: 'Text', value: 'On the device that sent it' });
});

test('what a received item offers follows §6.2: Decline while received, Close once started, Open on every one that has not ended', () => {
  assert.deepEqual(itemActions(item()).map(action => action.label), ['Open…', 'Decline']);
  const started = item({ state: 'started', offers: ['closed'], opens: [claimed()] });
  assert.deepEqual(itemActions(started).map(action => action.label), ['Open again…', 'Close handoff']);
  assert.equal(itemActions(started)[0].primary, false);
  assert.deepEqual(itemActions(item({ state: 'declined', offers: [], open_offered: false })), []);
  // Unlinked (Q31): a received handoff keeps Open and nothing else.
  assert.deepEqual(itemActions(item({ link_ended: true, offers: [] })).map(action => action.id), ['open']);
});

test('a handoff that ended before it reached this device has no title, no text and no Open', () => {
  const expired = item({ title: '', has_text: false, ever_held: false, state: 'expired', offers: [], open_offered: false });
  assert.equal(neverArrived(expired), true);
  assert.equal(itemTitle(expired), 'From Daniel Reyes');
  assert.equal(stateLine(item()), '', 'who it is from is the From row, said once');
  assert.equal(stateLine(expired), 'expired before it reached this device');
  assert.equal(stateLine({ ...expired, state: 'withdrawn' }), 'withdrawn before it reached this device');
  assert.deepEqual(itemActions(expired), []);
  // A copy this device held and a withdrawal erased is a different thing.
  const erased = item({ has_text: false, ever_held: true, state: 'withdrawn', offers: [], open_offered: false });
  assert.equal(neverArrived(erased), false);
  assert.equal(stateLine(erased), 'Daniel Reyes withdrew this handoff');
});

test('a row opened on another device whose text never reached this one says so, and offers Close without Open', () => {
  const elsewhere = item({ title: '', has_text: false, ever_held: false, state: 'opened', offers: ['closed'],
    open_offered: false, open_withheld_code: 'no_text' });
  assert.equal(neverArrived(elsewhere), true);
  assert.equal(stateView(elsewhere).word, 'opened');
  assert.equal(stateLine(elsewhere), 'opened on another of your devices; the text is no longer on the team server');
  assert.equal(stateLine({ ...elsewhere, state: 'started' }), 'opened on another of your devices; the text is no longer on the team server');
  assert.deepEqual(itemActions(elsewhere).map(action => action.id), ['close']);
  // Once it ends, the row says how it ended, never that it expired.
  assert.equal(stateLine({ ...elsewhere, state: 'closed', offers: [] }), 'you closed it');
  assert.equal(stateLine({ ...elsewhere, state: 'declined', offers: [] }), 'you declined it');
  assert.equal(stateLine({ ...elsewhere, state: 'expired', offers: [] }), 'expired before it reached this device');
});

test('an Open reads as the plan words it; Retry is offered in one case only', () => {
  const waiting = openView(claimed(), labels);
  assert.equal(waiting.words, 'Started in Beta session 019a4c…e1; waiting for its next prompt');
  assert.deepEqual(waiting.actions.map(action => action.label), ['Go to session', 'Deliver again']);
  const delivered = openView(claimed({ brief: 'delivered', brief_due_again: true }), labels);
  assert.equal(delivered.words, 'Opened in Beta session 019a4c…e1');
  assert.equal(delivered.note, 'Delivers again with its next prompt.');
  assert.ok(delivered.actions.some(action => action.id === 'deliver-again'), 'Deliver again is on an opened item too');
  const lost = openView(claimed({ brief: 'not_delivered' }), labels);
  assert.equal(lost.words, 'Started in Beta session 019a4c…e1; brief not delivered');
  // A handoff that ended offers nothing on its opens but the way to the session.
  assert.deepEqual(openView(claimed({ offers: [] }), labels).actions.map(action => action.id), ['go']);
  const notStarted = openView({ runtime: 'beta', state: 'cancelled', ended_code: 'runtime_not_started', ended_detail: 'exec: "beta": not found', offers: ['retry'] }, labels);
  assert.equal(notStarted.words, 'Could not start Beta');
  assert.equal(notStarted.note, 'exec: "beta": not found');
  assert.deepEqual(notStarted.actions.map(action => action.label), ['Retry']);
  const noHook = openView({ runtime: 'beta', state: 'cancelled', ended_code: 'hook_did_not_run', offers: [] }, labels);
  assert.equal(noHook.words, 'The Beta hook did not run in the session Crossing Guard started');
  assert.ok(!noHook.actions.some(action => action.id === 'retry'), 'the hook case names its fix and offers no Retry');
  assert.deepEqual(noHook.actions.map(action => action.id), ['runtimes']);
  assert.equal(openView({ runtime: 'beta', state: 'cancelled', ended_code: 'link_ended', offers: [] }, labels).words, 'Cancelled when this device left the team');
  // After the handoff ends: a session that started for it is still shown, with nothing
  // offered but the way to it; an Open that started none has no row.
  const kept = openView(claimed({ state: 'cancelled', ended_code: 'handoff_withdrawn', brief: 'delivered', offers: [] }), labels, { ended: true });
  assert.deepEqual([kept.words, kept.actions.map(action => action.id)], ['Opened in Beta session 019a4c…e1', ['go']]);
  assert.equal(openView({ runtime: 'beta', state: 'cancelled', ended_code: 'runtime_not_started', offers: [] }, labels, { ended: true }), null);
  // An open composer, or one closed without sending, is not a fact about the handoff.
  assert.equal(openView({ runtime: 'beta', state: 'waiting', offers: [] }, labels), null);
  assert.equal(openView({ runtime: 'beta', state: 'cancelled', ended_code: 'composer_closed', offers: [] }, labels), null);
});

test('the open sheet says Ready, or exactly what this device has not seen', () => {
  const ready = readinessView({ runtime: 'alpha', ready: true, missing: [], recall_tools: 'current', memory_recall: { observed_injecting: true } }, labels, '30 days');
  assert.deepEqual([ready.label, ready.ready, ready.steps, ready.notes], ['Alpha', true, ['Ready'], []]);
  const missing = readinessView({ runtime: 'beta', ready: false, missing: ['session_entry_not_observed', 'prompt_event_not_observed'] }, labels, '30 days');
  assert.deepEqual(missing.steps, ['No session start seen in 30 days', 'No prompt seen in 30 days']);
  assert.equal(readinessView({ runtime: 'beta', ready: false, missing: ['hook_did_not_run_in_launched_session'] }, labels).steps[0],
    'Its hook did not run in the session Crossing Guard started');
  assert.equal(readinessView({ runtime: 'beta', ready: false, missing: ['prompt_kind_not_a_carrier_kind'] }, labels).steps[0], 'Its prompt event cannot carry a handoff here');
  // Recall off, or the recall tools absent, never withholds Open: they are notes.
  const off = readinessView({ runtime: 'alpha', ready: true, missing: [], recall_tools: 'absent', memory_recall: { observed_injecting: false, entry_present: false } }, labels);
  assert.deepEqual(off.notes, ['Memory recall off', 'The handoff tool is not registered: the session gets the summary only']);
  assert.equal(readinessView({ runtime: 'alpha', ready: true, recall_tools: 'current', memory_recall: { observed_injecting: false, entry_present: true } }, labels).notes[0],
    'Memory recall on, not yet observed');
  assert.equal(firstReady([missing, ready]), 'alpha');
  assert.equal(firstReady([missing]), '');
  assert.equal(windowWords('720h0m0s'), '30 days');
  assert.equal(windowWords('36h0m0s'), '36 hours');
  assert.equal(windowWords(''), '');
});

test('the rail group: Received and Sent, and one count', () => {
  const list = withSide({ received: [item(), item({ id: 'hnd_2' })], sent: [{ ...sent(), side: undefined }] });
  const group = railGroup(list);
  assert.deepEqual([group.total, group.received.length, group.sent.length, group.empty], [3, 2, 1, false]);
  assert.equal(group.sent[0].side, 'sent');
  assert.equal(railGroup(withSide({ received: [], sent: [] })).empty, true);
  assert.equal(railMeta(item()), 'from Daniel Reyes');
  assert.equal(railMeta(sent()), 'to Priya Natarajan');
  assert.equal(railMeta(item({ local: true })), 'this device');
  assert.equal(railMeta(item({ title: '', ever_held: false, state: 'expired' })), '', 'a row named by its sender does not say the sender twice');
  assert.equal(railWhen('2026-10-04T10:13:00Z', { locale: 'en-US', timeZone: 'UTC' }), 'Oct 4, 10:13 AM');
  assert.equal(railWhen(''), '');
});

// Journey R-2: a closed handoff sat above a newer received one. What needs the
// person is listed first; the rest follow by the most recent change of state.
test('the rail lists what needs the person first, then the rest by the most recent change', () => {
  const at = minute => '2026-10-04T10:' + String(minute).padStart(2, '0') + ':00Z';
  const received = [
    item({ id: 'closed-new', state: 'closed', state_at: at(50) }),
    item({ id: 'opened', state: 'opened', state_at: at(20) }),
    item({ id: 'received-old', state: 'received', state_at: at(5) }),
    item({ id: 'declined-old', state: 'declined', state_at: at(10) }),
    item({ id: 'started', state: 'started', state_at: at(30) }),
    item({ id: 'received-new', state: 'received', state_at: at(40) }),
  ];
  const sentItems = [
    sent({ id: 'withdrawn', state: 'withdrawn', state_at: at(55) }),
    sent({ id: 'sent', state: 'sent', state_at: at(15) }),
    sent({ id: 'refused', state: 'refused', state_at: at(10) }),
    sent({ id: 'opened-by-them', state: 'opened', state_at: at(45) }),
    sent({ id: 'queued', state: 'queued', state_at: at(25) }),
    sent({ id: 'no-time', state: 'closed', state_at: '', created_at: '' }),
  ];
  const list = withSide({ received, sent: sentItems });
  const group = railGroup(list, { showEnded: true });
  assert.deepEqual(group.received.map(row => row.id), ['received-new', 'received-old', 'started', 'opened', 'closed-new', 'declined-old']);
  assert.deepEqual(group.sent.map(row => row.id), ['queued', 'refused', 'withdrawn', 'opened-by-them', 'sent', 'no-time']);
  assert.deepEqual(list.received.map(row => row.id), received.map(row => row.id), 'the list read from the daemon is left as it was');
});

// Journey R-3: the group listed every handoff ever, closed included, above the
// repositories. What has not ended is always a row; of the ended ones the few most
// recently ended are rows (the daemon publishes how many); the rest are one row away.
test('the rail shows what has not ended and the most recently ended; the rest stand behind one row', () => {
  const at = minute => '2026-10-04T10:' + String(minute).padStart(2, '0') + ':00Z';
  const list = withSide({
    received: [
      item({ id: 'r-received', state: 'received', state_at: at(1) }),
      item({ id: 'r-opened', state: 'opened', state_at: at(2) }),
      item({ id: 'r-closed-30', state: 'closed', state_at: at(30) }),
      item({ id: 'r-declined-10', state: 'declined', state_at: at(10) }),
      item({ id: 'r-expired-50', state: 'expired', state_at: at(50) }),
    ],
    sent: [
      sent({ id: 's-refused', state: 'refused', state_at: at(3) }),
      sent({ id: 's-sent', state: 'sent', state_at: at(4) }),
      sent({ id: 's-withdrawn-40', state: 'withdrawn', state_at: at(40) }),
      sent({ id: 's-closed-20', state: 'closed', state_at: at(20) }),
    ],
  });
  const ids = group => [group.received.map(row => row.id), group.sent.map(row => row.id)];

  // The two most recently ended are rows, whichever side they are on; a refused send is not ended, it waits on the person.
  const folded = railGroup(list, { endedVisible: 2 });
  assert.deepEqual(ids(folded), [['r-received', 'r-opened', 'r-expired-50'], ['s-refused', 's-withdrawn-40', 's-sent']]);
  assert.deepEqual([folded.total, folded.receivedCount, folded.sentCount, folded.endedBehind], [9, 5, 4, 3], 'the counts are of the lists, not of the rows');
  assert.equal(endedRowWords(folded), 'Show ended (3)');

  // Shown: every item, in the rail's order, and the row offers to put them away again.
  const open = railGroup(list, { endedVisible: 2, showEnded: true });
  assert.deepEqual(ids(open), [['r-received', 'r-opened', 'r-expired-50', 'r-closed-30', 'r-declined-10'], ['s-refused', 's-withdrawn-40', 's-closed-20', 's-sent']]);
  assert.equal(endedRowWords(open), 'Hide ended (3)');

  // The number not read yet: nothing stands in for it, so no ended row is shown and all are one row away.
  const unread = railGroup(list);
  assert.deepEqual(ids(unread), [['r-received', 'r-opened'], ['s-refused', 's-sent']]);
  assert.equal(endedRowWords(unread), 'Show ended (5)');
  // Fewer ended than may be shown: there is no row to show or hide.
  assert.equal(endedRowWords(railGroup(list, { endedVisible: 5 })), '');
  assert.equal(railGroup(list, { endedVisible: 5 }).received.length, 5);
});

test('recipients are names only; the person’s own entry is their other devices', () => {
  const choices = recipientChoices([{ ...DANIEL, self: true }, PRIYA, { user_id: 'usr_maya', display_name: 'Maya Okafor' }]);
  assert.deepEqual(choices.map(choice => choice.label), ['Maya Okafor', 'Priya Natarajan', 'Daniel Reyes — your other devices']);
  assert.deepEqual(choices.map(choice => choice.value), ['usr_maya', 'usr_priya', 'usr_daniel']);
});

test('the sheet previews the form and sends only that preview back', () => {
  const form = draftForm({ title: ' Rules 4–6 remain ', markdown: 'body', remaining: ['Rule 4', ' ', 'Rule 5'], session_ref: 'alpha/c-1' });
  assert.deepEqual(form, { to: '', title: 'Rules 4–6 remain', remaining: ['Rule 4', 'Rule 5'], text: 'body', includeConversation: false });
  assert.equal(formProblem(form), 'Choose who it goes to.');
  assert.equal(formProblem(form, true), '', 'a same-device handoff has no recipient');
  form.to = 'usr_priya';
  assert.equal(formProblem(form), '');
  assert.equal(formProblem({ ...form, title: ' ' }), 'Give it a title.');
  assert.equal(formProblem({ ...form, text: '' }), 'The text is empty.');
  const session = { runtime: 'alpha', id: 'c-1' };
  const preview = formRequest(form, session);
  assert.deepEqual(preview, { runtime: 'alpha', session_id: 'c-1', to: 'usr_priya', local: false, title: 'Rules 4–6 remain', body_markdown: 'body',
    remaining: ['Rule 4', 'Rule 5'], include_conversation: false, preview: true });
  const answer = { id: 'hnd_9', created_at: '2026-10-04T10:12:00Z', wire_hash: 'sha256:abc' };
  const real = formRequest(form, session, { previewed: answer });
  assert.deepEqual([real.preview, real.id, real.created_at, real.wire_hash], [false, 'hnd_9', '2026-10-04T10:12:00Z', 'sha256:abc']);
  assert.deepEqual(formRequest(form, session, { local: true }).to, '', 'a same-device handoff names no recipient');
  // Any edit makes the preview stale: Send is offered only from a fresh one.
  const key = formKey(form);
  assert.equal(formKey({ ...form }), key);
  for (const edit of [{ title: 'x' }, { text: 'y' }, { remaining: ['Rule 4'] }, { includeConversation: true }, { to: 'usr_maya' }]) {
    assert.notEqual(formKey({ ...form, ...edit }), key, JSON.stringify(edit));
  }
  assert.notEqual(formKey(form, true), key);
});

test('"what will be sent" is the preview as readable fields with a byte count; the excerpt is folded to three facts', () => {
  const preview = { preview: true, title: 'Rules 4–6 remain', remaining: ['Rule 4', 'Rule 5'], body_markdown: 'Notes are in [absolute path].',
    agents: [{ profile_id: 'scope-watch', name: 'Scope watch' }], bytes: 9870, checks: { redactions: { 'github-token': 1 }, redaction_count: 1, absolute_paths: 2 },
    conversation: { turn_count: 2, bytes: 61, truncated: true, turns: [{ seq: 1, role: 'user', text: 'token [redacted:github-token]' }, { seq: 2, role: 'assistant', text: 'see [absolute path] and [absolute path]' }] } };
  const model = willBeSent(preview);
  assert.equal(model.bytes, '9,870 bytes');
  assert.deepEqual([model.title, model.remaining, model.text, model.agents], ['Rules 4–6 remain', ['Rule 4', 'Rule 5'], 'Notes are in [absolute path].', ['Scope watch']]);
  assert.deepEqual(model.excerpt.facts, [{ label: 'Turns', value: '2 turns (older ones did not fit)' }, { label: 'Size', value: '61 bytes' }, { label: 'Replaced', value: '3 secrets or paths' }]);
  assert.equal(model.excerpt.turns.length, 2, 'it opens to every turn as it will leave');
  assert.deepEqual(model.checks, [{ label: 'Secrets replaced', value: '1 match (github-token)', changed: true }, { label: 'Paths replaced', value: '2 paths outside the repository', changed: true }]);
  // Red-team M3: the session's tags (with the water mark) and its identities leave
  // in the document, so the sheet shows them, one fact per line.
  assert.deepEqual([model.sessionTags, model.session], [[], []], 'a preview with neither draws neither');
  const carried = willBeSent({ ...preview, governance_state: { water_mark: 'confidential', tags: [{ key: 'data-class', value: 'confidential' }, { key: 'reviewed', value: 'yes' }] },
    session: { id: 'ses_wire', runtime: 'fixture-runtime', native_id: 'native-1', catalog_id: 'catalog-1', resume_id: 'resume-1' } });
  assert.deepEqual(carried.sessionTags, [{ label: 'Highest data class', value: 'confidential' }, { label: 'data-class', value: 'confidential' }, { label: 'reviewed', value: 'yes' }]);
  assert.deepEqual(carried.session, [{ label: 'Runtime', value: 'fixture-runtime' }, { label: 'Session id', value: 'native-1' }, { label: 'Catalog id', value: 'catalog-1' },
    { label: 'Resume id', value: 'resume-1' }, { label: 'Id on the team server', value: 'ses_wire' }]);
  const clean = willBeSent({ title: 't', remaining: [], body_markdown: 'b', agents: [], bytes: 1, checks: { redactions: {}, redaction_count: 0, absolute_paths: 0 } });
  assert.equal(clean.excerpt, null, 'no excerpt unless it was ticked');
  assert.equal(clean.bytes, '1 byte');
  assert.deepEqual(clean.checks, [{ label: 'Checked', value: 'No secrets and no paths outside the repository were found.' }]);
  assert.equal(willBeSent(null), null);
  assert.equal(bytesWords(1412), '1,412 bytes');
});

test('the sheet carries both sentences; a same-device handoff says only that it stays', () => {
  const both = sheetSentences({ recipient: 'Priya Natarajan' });
  assert.equal(both.length, 2);
  assert.equal(both[0], 'Priya Natarajan chooses the runtime; this text may be sent to whichever model provider their device uses.');
  assert.equal(both[1], 'Whoever runs the team server can read this text.');
  assert.ok(sheetSentences({}).at(0).startsWith('The recipient chooses the runtime'));
  assert.deepEqual(sheetSentences({ local: true }), ['Stays on this device.']);
});

test('a refused send says why in words and that the text is kept', () => {
  assert.equal(sendRefusal({ code: 'recipient_inbox_full', message: 'x' }, 'Priya Natarajan'), 'Not sent: Priya Natarajan has too many unopened handoffs. Your text is kept.');
  assert.equal(sendRefusal({ code: 'recipient_inactive' }, ''), 'Not sent: The recipient is no longer a member of this team. Your text is kept.');
  assert.match(sendRefusal({ code: 'preview_mismatch' }), /^The session moved on/);
  assert.equal(sendRefusal({ code: 'invalid_handoff', message: 'title: too long' }), 'Not sent: title: too long. Your text is kept.');
});

test('the document’s facts are one per row, and its agents say whether each is adopted here', () => {
  const record = { conversation: { turns: [{ seq: 1 }, { seq: 2 }] }, agents: [{ profile_id: 'scope-watch', name: 'Scope watch' }, { profile_id: 'notes', name: 'Notes' }] };
  const facts = documentFacts(item({ repository_id: 'remote-sha256-v1:9276', session: { runtime: 'alpha' } }), record,
    { when: iso => 'at ' + iso, runtimeLabel: labels, folder: '/work/limiter' });
  assert.deepEqual(facts.map(row => row.label), ['From', 'Repository', 'On this device', 'Written in', 'Excerpt', 'Sent', 'Received']);
  assert.deepEqual(facts.map(row => row.value), ['Daniel Reyes', 'limiter', '/work/limiter', 'Alpha', 'Their last 2 turns', 'at 2026-10-04T10:12:00Z', 'at 2026-10-04T10:13:00Z']);
  // The wire names a repository by an id, not a name: it is never shown.
  const nowhere = documentFacts(item({ repository_id: 'remote-sha256-v1:9276' }), record, { folderKnown: true });
  assert.equal(nowhere.find(row => row.label === 'Repository').value, 'No checkout of it on this device');
  assert.ok(!JSON.stringify(nowhere).includes('remote-sha256'));
  for (const row of facts) assert.ok(!row.value.includes(' · '), 'no fact is chained with a dot: ' + row.value);
  assert.equal(documentFacts(item({ local: true }), {}).find(row => row.label === 'From').value, 'A session on this device');
  assert.deepEqual(documentFacts(sent({ state: 'refused', refusal_code: 'rate_limited' }), {}, { when: () => 'then' }).map(row => row.label), ['To', 'Excerpt', 'Written']);
  assert.equal(documentFacts(item({ link_ended: true }), {}).find(row => row.label === 'Team').value, 'This device is no longer linked to it');
  assert.equal(documentFacts(item(), {}).find(row => row.label === 'Excerpt').value, 'Not included');
  const adopted = adoptedProfileIds({ adopted_bundles: [{ agents: [{ profile_id: 'scope-watch' }] }, { agents: null }] });
  assert.deepEqual(agentRows(record.agents, adopted), [{ name: 'Scope watch', adopted: true }, { name: 'Notes', adopted: false }]);
  assert.deepEqual(agentRows(record.agents, adoptedProfileIds(null)).map(row => row.adopted), [false, false]);
  assert.equal(receiptLine(item({ receipt_code: 'handoff_withdrawn' })), 'The team server did not take the last change: it had already been withdrawn.');
  assert.equal(receiptLine(item()), '');
});

test('an opened session says what it continues, and both sessions name the other under Related sessions', () => {
  const facts = { opened: true, handoff_id: 'hnd_1', title: 'Rules 4–6 remain', from: DANIEL };
  assert.equal(continuesLine(facts), 'continues “Rules 4–6 remain” from Daniel Reyes');
  assert.equal(continuesLine({ opened: true, title: 'x', local: true }), 'continues “x” from an earlier session on this device');
  assert.equal(continuesLine({ opened: false }), '');
  const session = { runtime: 'alpha', id: 'c-1', resume_id: 'r-1', identities: ['n-1'] };
  assert.deepEqual(relatedHandoffRows({ session, facts }), [{ label: 'Daniel Reyes’s session', description: 'handed off “Rules 4–6 remain”' }]);
  const continued = sent({ state: 'opened', opened_by: { session: { runtime: 'beta' } } });
  assert.deepEqual(relatedHandoffRows({ session, list: { sent: [continued] }, runtimeLabel: labels }), [{ label: 'Priya Natarajan’s session', description: 'continued this in Beta' }]);
  // Not continued yet, sent from another session, or the same id under another runtime: no row.
  assert.deepEqual(relatedHandoffRows({ session, list: { sent: [sent()] } }), []);
  assert.deepEqual(relatedHandoffRows({ session: { runtime: 'alpha', id: 'other', resume_id: 'other' }, list: { sent: [continued] } }), []);
  assert.deepEqual(relatedHandoffRows({ session: { ...session, runtime: 'beta' }, list: { sent: [continued] } }), []);
  assert.deepEqual(nativeIdsOf({ id: 'c-1', resume_id: 'r-1' }), ['r-1', 'c-1']);
  assert.deepEqual(nativeIdsOf({ id: 'same', resume_id: 'same' }), ['same']);
  assert.equal(shortSession('short'), 'short');
});

test('a context row that is the hook’s brief reads "handoff"; any other keeps the hook’s event name', () => {
  assert.equal(contextRowName({ kind: 'context', name: 'UserPromptSubmit', text: '[Crossing Guard handoff from your teammate Daniel Reyes: a teammate\'s text' }), 'handoff');
  assert.equal(contextRowName({ kind: 'context', name: 'SessionStart', text: 'Memory index' }), 'SessionStart');
  assert.equal(contextRowName({}), '');
});

test('memory recall: three state words, one action, and the words when this daemon may not act', () => {
  const state = extra => ({ runtime: 'alpha', observed_injecting: false, entry_present: false, added_by_action: false, config_path: '/home/p/.alpha/settings.json', offers: [], ...extra });
  const off = recallView(state({ offers: ['attach'] }), labels);
  assert.deepEqual([off.word, off.chip, off.action, off.note], ['off', 'st-stale', { id: 'attach', label: 'Turn on memory recall' }, 'Alpha sessions get no memory index.']);
  const unseen = recallView(state({ entry_present: true, added_by_action: true, offers: ['detach'] }), labels);
  assert.deepEqual([unseen.word, unseen.action, unseen.note], ['on — not yet observed', { id: 'detach', label: 'Turn off memory recall' }, '']);
  assert.equal(recallView(state({ entry_present: true, added_by_action: true, observed_injecting: true, offers: ['detach'] }), labels).word, 'on — observed injecting');
  // An entry the person added by hand is left alone: no action, and it says so.
  const hand = recallView(state({ entry_present: true, observed_injecting: true }), labels);
  assert.deepEqual([hand.action, hand.note], [null, 'Added by hand — left alone.']);
  // OD-24: a daemon that does not own the installation offers nothing and says why.
  const foreign = recallView(state({ withheld_code: 'not_this_installation', withheld: 'this home\'s alpha hook runs /usr/local/bin/crossing-guard, not this daemon' }), labels);
  assert.equal(foreign.action, null);
  assert.equal(foreign.note, 'This home\'s alpha hook runs /usr/local/bin/crossing-guard, not this daemon.');
  assert.equal(recallView(state({ withheld_code: 'no_lifecycle_hook', withheld: 'this home has no Crossing Guard hook for alpha' }), labels).note,
    'Alpha is not connected on this device. Connect it first.');
  const consent = recallConsent(off, 'attach');
  assert.equal(consent.title, 'Turn on memory recall for Alpha');
  assert.deepEqual(consent.rows[0], { label: 'File', value: '/home/p/.alpha/settings.json' }, 'the consent names the file it edits');
  assert.equal(recallConsent(unseen, 'detach').label, 'Turn off memory recall');
  assert.equal(recallOutcome({ changed: true }), '');
  assert.match(recallOutcome({ changed: false, code: 'not_added_by_action' }), /added by hand/);
  assert.match(recallOutcome({ changed: false, code: 'already_present' }), /already there/);
});

test('no handoff surface uses the words ticket, receipt, cursor or outbox', () => {
  const here = dirname(fileURLToPath(import.meta.url));
  const banned = /\b(ticket|receipt|cursor|outbox)s?\b/i;
  for (const name of readdirSync(here).filter(file => file.endsWith('.js'))) {
    const source = readFileSync(join(here, name), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
    // Words a person can read are string literals that hold a space or start a sentence.
    for (const [, literal] of source.matchAll(/'((?:[^'\\\n]|\\.)*)'/g)) {
      if (!/\s/.test(literal) && !/^[A-Z]/.test(literal)) continue; // an identifier, a route segment, a field name
      assert.ok(!banned.test(literal), name + ' shows “' + literal + '”');
    }
  }
});

// Red-team Low 15: a member list that could not be read is not an empty team.
test('the sheet says the directory could not be read, not that the team is empty', () => {
  assert.equal(noRecipientWords({ linked: true, members: [], problem: 'the team server did not answer' }), 'The team’s member list could not be read: the team server did not answer');
  assert.equal(noRecipientWords({ linked: true, members: [] }), 'No teammates on this team yet.');
  assert.equal(noRecipientWords({ linked: false, problem: 'ignored' }), 'This device is not linked to a team.');
});

// Journey C-4: three opens on ONE device read "on 2 other devices". The count is of
// other sessions, wherever they ran.
test('“Also opened” counts other sessions and names no device', () => {
  const rows = count => documentFacts({ side: 'received', state: 'opened', created_at: '2026-10-04T10:00:00Z', other_opened_count: count }, null);
  assert.deepEqual(rows(2).find(row => row.label === 'Also opened'), { label: 'Also opened', value: 'in 2 other sessions' });
  assert.deepEqual(rows(1).find(row => row.label === 'Also opened'), { label: 'Also opened', value: 'in 1 other session' });
  assert.equal(rows(0).find(row => row.label === 'Also opened'), undefined);
  assert.equal(JSON.stringify(rows(2)).includes('device'), false);
});

// Journey W-2: Settings → Runtimes said "sessions get no memory index" while the
// daemon reported it had observed the index being injected. Observed wins.
test('memory recall observed injecting is never worded as off', () => {
  const view = recallView({ runtime: 'fixture-runtime', entry_present: false, observed_injecting: true, offers: ['attach'] }, () => 'Fixture');
  assert.deepEqual([view.word, view.chip], ['on — observed injecting', 'st-verified']);
  assert.equal(view.note.includes('get no memory index'), false);
  assert.ok(view.note.includes('received the memory index'));
  const off = recallView({ runtime: 'fixture-runtime', entry_present: false, observed_injecting: false, offers: ['attach'] }, () => 'Fixture');
  assert.deepEqual([off.word, off.note], ['off', 'Fixture sessions get no memory index.']);
});
