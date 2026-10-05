// Settings → Team's rules as tests (team rest-of-release plan §4.2, criterion 89, §14
// Q6, Q31, Q33, Q34 and the sidebar-count decision). Fixtures are shaped like the Go
// response types of GET /api/team and GET /api/team/layers.
import test from 'node:test';
import assert from 'node:assert/strict';
import { teamPage, teamAttention, teamDetails, unadoptWords, unlinkWords, unlinkOutcome, approvalHref, contentWords, expiryWords, bundleTitle, cadenceWords } from './settings-team-model.js';
import { settingsAttention, attentionCounts } from './settings-model.js';

const DEVICE_ID = 'dev_01JB7M2W4K', DEVICE_PRINT = 'SHA256:Lm3T8QwE2RnVx9Q4', SERVER = 'https://team.harborstreet.example';
const PINNED = 'SHA256:7Q2M9KXD4TRBW6HC', PRESENTED = 'SHA256:9F4KC2WN8HTER5QA', DIGEST = 'sha256:aa11bb22cc33';
const options = { locale: 'en-GB', timeZone: 'UTC' };

function linkedStatus(extra = {}) {
  return { state: 'linked', server: SERVER, organization: { id: 'org_1', name: 'Harbor Street Labs' },
    device: { id: DEVICE_ID, name: 'Priya’s MacBook Pro', platform: 'darwin/arm64', fingerprint: DEVICE_PRINT },
    linked_at: '2026-09-28T09:02:00Z', approved_by: 'Maya Okafor',
    report: { last_at: '2026-10-04T10:38:00Z', outcome: 'ok', interval: '5m0s', document: { device_id: DEVICE_ID } },
    sync: { content: 'off' }, sends: ['events: verb, tool, target'],
    outbox: { pending: 0, by_kind: {}, high_water: 10000, over_high_water: false, parked: [], interval: '30s', batch: 50,
      last_at: '2026-10-04T10:39:00Z', outcome: 'ok', accepted: 4211, dead_letter: {}, refused: {}, conflicts: {} },
    content: { opted_in: [], mandate: null, session_cap: 1048576, retention: 'turning content off stops what has not been sent' },
    memory: { shared: 1, pulled: 12, share_candidates: 0, pull: { last_at: '2026-10-04T10:40:00Z', outcome: 'ok', cursor: 9, interval: '15m0s', unlandable: 0 } },
    ...extra };
}

function record(extra = {}) {
  return { organization_id: 'org_1', organization_name: 'Harbor Street Labs', scope: 'organization', bundle_id: 'bnd_r4', revision: 4, key_id: 'ok_7Q2M',
    failure_mode: 'fail-open', expires_at: '2026-11-03T00:00:00Z', adopted_at: '2026-09-28T09:20:00Z', signed_digest: DIGEST, rulebook_digest: DIGEST,
    schema_version: '1.1', linked: true, expired: false, blocking: false, rule_count: 7,
    agents: [{ profile_id: 'scope-watch', name: 'Scope watch', source_digest: DIGEST, bundle_digest: DIGEST }], ...extra };
}

function offer(extra = {}) {
  return { id: 'bnd_r5', organization_id: 'org_1', scope: 'organization', revision: 5, schema_version: '1.1', key_id: 'ok_7Q2M',
    expires_at: '2026-10-28T00:00:00Z', failure_mode: 'fail-open', adopted: false, status: 'offered', state_token: DIGEST,
    rules: [{ id: 'no-force-push', action: 'deny', intent: 'force pushes to protected branches' }, { id: 'prod-migrations', action: 'ask' }],
    documents: [
      { kind: 'rulebook', name: 'rules.json', digest: DIGEST, media_type: 'application/json', state: 'offered' },
      { kind: 'profile', name: 'scope-watch/PROFILE.md', digest: DIGEST, state: 'offered', profile_id: 'scope-watch', profile_name: 'Scope watch', change: 'new', text: 'You watch a coding session.' },
      { kind: 'profile', name: 'release-notes/PROFILE.md', digest: DIGEST, state: 'id_in_use', profile_id: 'release-notes', profile_name: 'Release notes', change: 'collision', text: 'Write release notes.' },
      { kind: 'detectors', name: 'detectors.json', digest: DIGEST, state: 'not_applied', reason: 'shared detectors are not applied by this version' }],
    ...extra };
}

function layers(extra = {}) {
  return { available: [], adopted: {}, adopted_bundles: [], unusable: [], key_mismatches: [],
    pinned_org_key: { key_id: 'ok_7Q2M', public_key: 'cHVi', fingerprint: PINNED, pinned_at: '2026-09-28T09:14:00Z' }, ...extra };
}

const ACT_KEYS = new Set(['action', 'actions']);
const UNSEEN_KEYS = new Set(['technical', 'organizationId', 'kind', 'severity', 'tone', 'mode', 'inline', 'op', 'scope']);

// visible gathers every string the default view draws from a page: all of it except
// the technical text behind a disclosure, and of an act only its label.
function visible(value, key = '') {
  if (UNSEEN_KEYS.has(key) || value == null) return [];
  if (ACT_KEYS.has(key)) return [].concat(value).filter(Boolean).map(action => action.label);
  if (typeof value === 'string') return [value];
  if (typeof value !== 'object') return [];
  if (Array.isArray(value)) return value.flatMap(item => visible(item, key));
  return Object.entries(value).flatMap(([name, item]) => visible(item, name));
}

const CADENCES = [/\bevery \d/i, /\b\d+m\d+s\b/, /\b30 ?s\b/, /\b\d+ min\b/, /at a time/];
function assertNoMachinery(page, label) {
  const text = visible(page).join('\n');
  for (const secret of [DEVICE_ID, DEVICE_PRINT, SERVER, 'team.harborstreet.example', DIGEST, PINNED, 'ok_7Q2M', 'bnd_r', 'org_1']) {
    assert.ok(!text.includes(secret), label + ': the default view shows ' + secret);
  }
  for (const cadence of CADENCES) assert.ok(!cadence.test(text), label + ': the default view shows a cadence (' + cadence + ')');
  assert.ok(!/outbox|cursor|receipt|ticket/i.test(text), label + ': the default view uses a bookkeeping word');
}

test('linked and quiet: no needs-you block, the chip reads linked, and the sidebar counts nothing', () => {
  const page = teamPage(linkedStatus(), layers({ adopted_bundles: [record()], available: [offer({ id: 'bnd_r4', revision: 4, status: 'adopted', adopted: true,
    documents: offer().documents.filter(item => item.state !== 'id_in_use').map(item => ({ ...item, state: item.state === 'offered' ? 'adopted' : item.state })) })] }), options);
  assert.deepEqual(page.needs, []);
  assert.deepEqual(page.chip, { words: 'linked', tone: 'good' });
  assert.equal(page.heading, 'Linked to Harbor Street Labs');
  assert.deepEqual(page.team.map(row => [row.label, row.value]), [['Rules', '7 in force'], ['Shared agents', '1 adopted: Scope watch'], ['Shared memory', '12 records']]);
  assert.deepEqual(page.team[0].subs, ['Revision 4, valid until 3 Nov 2026']);
  assert.deepEqual(page.device.map(row => row.label), ['Session activity', 'Session content', 'Memory shared from here']);
  assert.deepEqual(teamAttention(linkedStatus(), layers({ adopted_bundles: [record()] })), []);
  assertNoMachinery(page, 'quiet');
});

test('an offer says what it shares in the person’s terms, each document’s state, and adopts from its own card', () => {
  const page = teamPage(linkedStatus(), layers({ available: [offer()] }), options);
  const [item] = page.needs;
  assert.equal(item.title, 'Harbor Street Labs shares 2 rules and 1 agent');
  assert.equal(item.sub, 'Nothing applies until you adopt.');
  assert.deepEqual(item.facts, [['Rules', '2'], ['Agents', 'Scope watch, off until you turn it on'], ['Not adopted', 'Release notes: your own agent already uses that id'],
    ['Session content', 'Not asked for'], ['If it expires', 'Its rules stop applying; its agents are held'], ['Valid until', '28 Oct 2026']]);
  assert.deepEqual(item.documents, [['2 rules', 'Adopted when you adopt'], ['Scope watch', 'Adopted when you adopt'],
    ['Release notes', 'Not adopted — your agent uses this id'], ['Detectors', 'Not applied by this version']]);
  assert.deepEqual(item.diffs.map(diff => diff.head), ['Rules: 2', 'Scope watch: new agent', 'Release notes: not adopted — your agent uses this id']);
  assert.equal(item.diffs[1].text, 'You watch a coding session.');
  assert.deepEqual(item.actions, [{ kind: 'adopt', label: 'Adopt', primary: true, scope: 'organization', bundle: 'bnd_r5', token: DIGEST }]);
  assert.equal(page.team[0].value, 'None adopted');
  assertNoMachinery(page, 'offered');
  const repository = teamPage(linkedStatus(), layers({ available: [offer({ scope: 'repository:harborstreet/rate-limiter', documents: [offer().documents[0]] })] }), options);
  assert.equal(repository.needs[0].title, 'Harbor Street Labs shares 2 rules for harborstreet/rate-limiter');
});

test('a changed bundle shows each changed signed field on its own block, the text diff, and no decline act', () => {
  const changed = offer({ status: 'changed', adopted_revision: 4, failure_mode: 'fail-closed',
    changes: [{ field: 'failure_mode', label: 'Failure mode', from: 'fail-open', to: 'fail-closed' },
      { field: 'content_policy', label: 'Content policy', from: '', to: '{"sync_content":"mandated"}' },
      { field: 'schema_version', label: 'Bundle format', from: '1.0', to: '1.1' }],
    rule_changes: { added: [], removed: ['prod-migrations'], changed: [{ id: 'no-force-push', before_action: 'ask', after_action: 'deny' }] },
    documents: [{ kind: 'profile', name: 'scope-watch/PROFILE.md', state: 'offered', profile_id: 'scope-watch', profile_name: 'Scope watch', change: 'update',
      text_diff: [{ op: 'keep', text: 'You watch.' }, { op: 'remove', text: 'Name them.' }, { op: 'add', text: 'List each file.' }, { op: 'add', text: 'One per line.' }] }] });
  const page = teamPage(linkedStatus(), layers({ adopted_bundles: [record()], available: [changed] }), options);
  const [item] = page.needs;
  assert.equal(item.title, 'Revision 5 changes what you adopted');
  assert.equal(item.sub, 'Still on revision 4 until you adopt.');
  assert.equal(item.inline, true);
  assert.deepEqual(item.facts, [['Rules', '1 changed, 1 removed'], ['Valid until', '28 Oct 2026']], 'an agent already adopted is not announced as off');
  assert.deepEqual(item.diffs.map(diff => diff.head), ['If it expires', 'Session content', 'Bundle format', 'Rules: 1 changed, 1 removed', 'Scope watch: instructions, 2 added, 1 removed']);
  assert.deepEqual(item.diffs[0].lines, [{ op: 'remove', text: 'Its rules stop applying; its agents are held' }, { op: 'add', text: 'Governed tool calls are blocked until it is refreshed' }]);
  assert.deepEqual(item.diffs[1].lines.map(line => line.text), ['Not asked for', 'Required: tool inputs of every session go to the team server']);
  assert.deepEqual(item.diffs[2].lines.map(line => line.text), ['1.0', '1.1']);
  assert.deepEqual(item.diffs[3].lines, [{ op: 'remove', text: 'no-force-push — ask' }, { op: 'add', text: 'no-force-push — deny' }, { op: 'remove', text: 'prod-migrations' }]);
  assert.deepEqual(item.actions.map(action => action.label), ['Adopt revision 5']);
  // The adopted revision stays in force and is what From the team shows.
  assert.deepEqual(page.team[0].subs, ['Revision 4, valid until 3 Nov 2026']);
  assertNoMachinery(page, 'changed');
});

test('a collision after adopting, a revision that can’t be used, and a partial adoption each get one item', () => {
  const adopted = offer({ id: 'bnd_r4', revision: 4, status: 'adopted', adopted: true });
  const unusable = { id: 'bnd_r6', organization_id: 'org_1', scope: 'organization', revision: 6, still_on_revision: 4, code: 'bundle_document_invalid',
    document: 'scope-watch/PROFILE.md', reason: 'agent document scope-watch/PROFILE.md does not parse: unknown signal', documents: [] };
  const page = teamPage(linkedStatus(), layers({ adopted_bundles: [record()], available: [adopted], unusable: [unusable] }), options);
  assert.deepEqual(page.needs.map(item => [item.kind, item.severity, item.title]),
    [['collision', 'info', 'Release notes was not adopted'], ['unusable', 'info', 'Revision 6 can’t be used']]);
  assert.equal(page.needs[1].sub, 'Still on revision 4. Only Harbor Street Labs’s lead can fix it.');
  assert.match(page.needs[1].technical, /unknown signal/);
  assert.deepEqual(page.needs[0].actions, [{ kind: 'agent', label: 'Your Release notes ›', agent: 'release-notes' }]);
  assert.deepEqual(page.team[1].subs, ['1 not adopted: Release notes, your own agent uses that id']);
  assert.ok(!visible(page).join('\n').includes('unknown signal'), 'the technical reason is behind a disclosure');
  const partial = teamPage(linkedStatus(), layers({ available: [offer({ status: 'partial' })] }), options);
  assert.deepEqual([partial.needs[0].title, partial.needs[0].actions[0].label], ['Adopting revision 5 did not finish', 'Finish adopting']);
});

test('an expired fail-open bundle holds its agents; a fail-closed one blocks, says so on the chip, and offers Un-adopt', () => {
  const held = teamPage(linkedStatus(), layers({ adopted_bundles: [record({ expired: true, expires_at: '2026-10-01T00:00:00Z' })] }), options);
  assert.deepEqual([held.needs[0].title, held.needs[0].sub], ['Rules and agents from Harbor Street Labs expired on 1 Oct 2026',
    'They come back by themselves when this device reaches the team server.']);
  assert.deepEqual(held.needs[0].facts, [['Rules', '7, not applied since 1 Oct 2026'], ['Scope watch', 'Held; it starts no run']]);
  assert.equal(held.chip.words, 'linked');
  assert.deepEqual([held.team[0].value, held.team[1].value], ['7, not applied', 'Scope watch, held']);
  const blocking = teamPage(linkedStatus(), layers({ adopted_bundles: [record({ expired: true, blocking: true, failure_mode: 'fail-closed', expires_at: '2026-10-01T00:00:00Z' })] }), options);
  const [item] = blocking.needs;
  assert.equal(item.title, 'Harbor Street Labs’s rules expired on 1 Oct 2026 and are blocking');
  assert.equal(item.sub, 'The next refresh lifts it with no action from you. Un-adopt lifts it at once.');
  assert.deepEqual(item.facts[0], ['Blocked', 'Every governed tool call on this device']);
  assert.deepEqual(item.actions.map(action => [action.kind, action.label]), [['unadopt', 'Un-adopt…']]);
  assert.deepEqual(blocking.chip, { words: 'rules expired — blocking', tone: 'bad' });
  assert.equal(blocking.team[0].value, '7, expired and blocking');
  assertNoMachinery(blocking, 'blocking');
});

test('a fingerprint leaves Details only for a new key to trust', () => {
  const mismatch = { pinned_key_id: 'ok_7Q2M', pinned_fingerprint: PINNED, presented_key_id: 'ok_9F4K', presented_fingerprint: PRESENTED, bundles: ['bnd_r5'] };
  const page = teamPage(linkedStatus(), layers({ adopted_bundles: [record()], key_mismatches: [mismatch] }), options);
  const [item] = page.needs;
  assert.deepEqual([item.kind, item.severity, item.title], ['key', 'bad', 'Harbor Street Labs has a new signing key']);
  assert.deepEqual(item.prints, [['Trusted now', PINNED], ['New', PRESENTED]]);
  assert.deepEqual(item.actions, [{ kind: 'trust', label: 'Trust new key…', primary: true, pinned: PINNED, presented: PRESENTED }]);
  const text = visible(page).join('\n');
  assert.ok(text.includes(PRESENTED) && text.includes(PINNED), 'both fingerprints are raised for the person to compare');
  assert.ok(!text.includes(DEVICE_PRINT) && !text.includes(DEVICE_ID) && !text.includes('ok_9F4K') && !text.includes('bnd_r5'));
  // Without a mismatch no fingerprint is anywhere in the default view, in any state.
  for (const [label, built] of [['quiet', teamPage(linkedStatus(), layers({ adopted_bundles: [record()] }), options)],
    ['unlinked', teamPage({ state: 'unlinked', device: { id: DEVICE_ID } }, layers({ adopted_bundles: [record({ linked: false })], pinned_org_key: {} }), options)]]) {
    assertNoMachinery(built, label);
  }
});

test('a link problem is one item written for the person, the chip, and a waiting-to-send row; the daemon’s words stay behind a disclosure', () => {
  const failing = linkedStatus({ report: { last_at: '2026-09-27T17:58:00Z', outcome: 'error', error: 'Post "' + SERVER + '/v1/report": dial tcp: connection refused', interval: '5m0s' } });
  failing.outbox = { ...failing.outbox, pending: 214, oldest_at: '2026-09-27T18:00:00Z', outcome: 'error', error: 'connection refused' };
  const page = teamPage(failing, layers(), options);
  assert.deepEqual(page.needs.map(item => [item.kind, item.title]), [['link', 'Harbor Street Labs’s server is not answering']]);
  assert.deepEqual(page.chip, { words: 'link problem', tone: 'bad' });
  const waiting = page.device.find(row => row.label === 'Waiting to send');
  assert.match(waiting.value, /^214 since 27 Sep/);
  assert.deepEqual(waiting.subs, ['The team server is not answering. They go out when it does.']);
  assertNoMachinery(page, 'link problem');
  const revoked = teamPage(linkedStatus({ state: 'revoked', problem: 'the server refused the device key' }), layers(), options);
  assert.deepEqual([revoked.needs[0].title, revoked.needs[0].technical, revoked.chip.words],
    ['The team server no longer accepts this device', 'the server refused the device key', 'link problem']);
  // Nothing waiting and nothing failing: no row at all.
  assert.equal(teamPage(linkedStatus(), layers(), options).device.find(row => row.label === 'Waiting to send'), undefined);
  // Waiting with no failure is a row, and not a needs-you item until the daemon flags it.
  const busy = linkedStatus(); busy.outbox = { ...busy.outbox, pending: 3, oldest_at: '2026-10-04T10:41:00Z' };
  assert.deepEqual(teamPage(busy, layers(), options).needs, []);
  assert.deepEqual(teamPage(busy, layers(), options).device.at(-1).subs, ['They go out with the next send.']);
  const over = linkedStatus(); over.outbox = { ...over.outbox, pending: 48721, over_high_water: true, conflicts: { memory: 2 } };
  assert.deepEqual(teamPage(over, layers(), options).needs.map(item => [item.kind, item.severity]), [['waiting', 'warn'], ['conflict', 'warn']]);
});

test('session content states the fact only: required, on for some sessions, or off', () => {
  const content = status => teamPage(status, layers(), options).device.find(row => row.label === 'Session content');
  assert.deepEqual(content(linkedStatus()), { label: 'Session content', value: 'Off', subs: ['Turned on per session, from the session'] });
  assert.equal(content(linkedStatus({ content: { opted_in: [{ session: 'r/1', since: '2026-10-04T10:00:00Z' }, { session: 'r/2', since: '2026-10-04T10:00:00Z' }], mandate: null } })).value, 'On for 2 sessions');
  assert.deepEqual(content(linkedStatus({ content: { opted_in: [], mandate: { bundle_id: 'bnd_r5', repositories: [] } } })),
    { label: 'Session content', value: 'On for every session', subs: ['Required by Harbor Street Labs’s rules'] });
  assert.equal(teamPage(linkedStatus({ memory: { shared: 1, pulled: 0, share_candidates: 3, pull: {} } }), layers(), options).device[2].action.label, 'Share 3 more…');
});

test('unlinked: the Link card, and what was adopted listed under its organization with Un-adopt', () => {
  const fresh = teamPage({ state: 'unlinked', device: { id: DEVICE_ID } }, layers({ pinned_org_key: {} }), options);
  assert.deepEqual([fresh.mode, fresh.heading, fresh.chip.words, fresh.needs.length, fresh.leftovers.length], ['unlinked', 'Not linked to a team', 'unlinked', 0, 0]);
  assert.deepEqual(fresh.device, [{ label: 'Leaves this machine', value: 'Nothing', subs: [] }]);
  const left = teamPage({ state: 'unlinked', device: { id: DEVICE_ID } }, layers({ adopted_bundles: [record({ linked: false })], available: [offer()] }), options);
  assert.equal(left.needs.length, 0, 'an unlinked device is offered nothing');
  assert.deepEqual(left.leftovers.map(group => group.organization), ['Harbor Street Labs']);
  assert.deepEqual(left.leftovers[0].rows.map(row => [row.label, row.value, row.subs[0], row.action.label]), [
    ['Organization rules', '7 in force', 'Valid until 3 Nov 2026, then they stop applying', 'Un-adopt…'],
    ['Scope watch', 'Shared agent', 'Held after 3 Nov 2026', 'Agents ›']]);
  assert.deepEqual(left.bundles, [], 'From the team lists only the linked organization’s bundles');
  // Linked elsewhere: the other organization's adoption is still listed, under its name.
  const elsewhere = teamPage(linkedStatus({ organization: { id: 'org_2', name: 'Other Co' } }), layers({ adopted_bundles: [record({ linked: false })] }), options);
  assert.deepEqual([elsewhere.leftovers[0].organization, elsewhere.team[0].value], ['Harbor Street Labs', 'None shared yet']);
});

test('waiting for approval: one fact per row, and the only state that shows this device’s key and an address', () => {
  const page = teamPage({ state: 'pending', device: { id: DEVICE_ID }, pending: { user_code: 'KQ7M-2XVD', verification_url: SERVER + '/approve', fingerprint: DEVICE_PRINT,
    expires_at: '2026-10-04T11:40:00Z', server: SERVER, name: 'Priya’s MacBook Pro' } }, layers({ pinned_org_key: {} }), options);
  assert.deepEqual([page.mode, page.heading, page.chip.words], ['pending', 'Linking to team.harborstreet.example', 'waiting for approval']);
  assert.deepEqual(page.pending.rows.map(([label]) => label), ['Open', 'Code', 'This device’s key', 'Device name', 'Code expires']);
  assert.deepEqual(page.device, [{ label: 'Leaves this machine', value: 'Nothing until it is approved', subs: [] }]);
  assert.ok(!visible(page).join('\n').includes(DEVICE_ID));
});

test('Details holds every identifier and cadence, and everything the earlier page showed that the default view dropped', () => {
  const status = linkedStatus();
  status.outbox = { ...status.outbox, pending: 38, by_kind: { event: 30, memory: 8 }, oldest_at: '2026-10-04T09:12:00Z', over_high_water: true,
    parked: [{ kind: 'handoff', reason: 'unsupported by the server', next_probe_at: '2026-10-04T12:00:00Z' }],
    dead_letter: { memory: { organization_scope_admin_only: 2 } }, refused: { event: { secret_match: 1 } }, conflicts: { memory: 2 } };
  const details = teamDetails(status, layers({ adopted_bundles: [record()], reasons: ['bundle bnd_x not offered: revision 3 is not newer'] }),
    { deletions: [{ slug: 'vpn', acknowledged: 2, outstanding: 1, stale: 0, revoked: 0 }], retention: 'backups may keep a copy' }, options);
  const lines = Object.fromEntries(details.rows.map(row => [row.label, row.lines]));
  assert.deepEqual([lines['Team server'], lines['Device id'], lines['Device key'], lines['Organization key'], lines['Key id']],
    [[SERVER], [DEVICE_ID], [DEVICE_PRINT], [PINNED], ['ok_7Q2M']]);
  assert.deepEqual(lines['Sends'], ['Every 30 s, up to 50 at a time']);
  assert.deepEqual([lines['Pulls'], lines['Reports']], [['Every 15 min'], ['Every 5 min']]);
  assert.deepEqual(['30s', '15m0s', '1h30m0s', '', 'soon'].map(cadenceWords), ['30 s', '15 min', '1 h 30 min', '', 'soon']);
  assert.deepEqual(lines['Waiting, by kind'], ['30 event', '8 memory']);
  assert.match(lines['Kinds the server does not accept'][0], /^handoff: unsupported by the server, tried again /);
  assert.deepEqual(lines['Refused by the server'], ['memory: 2 organization_scope_admin_only']);
  assert.deepEqual(lines['Not sent from this device'], ['event: 1 secret_match']);
  assert.deepEqual(lines['Conflicts'], ['2 memory conflicted with what the server holds']);
  assert.deepEqual(lines['Waiting to send'].slice(0, 3), ['38 waiting', 'High-water mark 10,000', 'Over the high-water mark']);
  assert.deepEqual(details.rows.find(row => row.label === 'Waiting to send').meter, { pending: 38, mark: 10000, over: true });
  assert.deepEqual(lines['Memory deletions'], ['vpn: deleted on 2 devices (1 outstanding, 0 stale, 0 revoked)', 'backups may keep a copy']);
  assert.deepEqual(lines['Memory'], ['1 record shared with the team · 12 records from teammates']);
  assert.deepEqual(lines['What is sent'], ['events: verb, tool, target']);
  assert.match(lines['Adopted bundles'][0], /bnd_r4, signed by ok_7Q2M, sha256:/);
  assert.deepEqual(lines['Not offered'], ['bundle bnd_x not offered: revision 3 is not newer']);
  assert.match(details.document, /"device_id": "dev_01JB7M2W4K"/);
  for (const label of ['Device name', 'Platform', 'Linked', 'Approved by', 'Key trusted', 'Last report', 'Last pull', 'Reports', 'Last send', 'Session content']) {
    assert.ok(lines[label]?.length, 'Details has ' + label);
  }
  // Unlinked, Details still holds the device id, and nothing that needs a link.
  assert.deepEqual(teamDetails({ state: 'unlinked', device: { id: DEVICE_ID } }, layers({ pinned_org_key: {} }), null, options).rows.map(row => row.label), ['Device id']);
});

test('the Team sidebar count is the needs-you block: absent when it is empty, one per item when it is not', () => {
  const quiet = settingsAttention({ team: linkedStatus(), teamLayers: layers({ adopted_bundles: [record()] }) });
  assert.deepEqual(attentionCounts(quiet).pages.team, undefined);
  const reads = { team: linkedStatus(), teamLayers: layers({ adopted_bundles: [record()], available: [offer({ id: 'bnd_r4', revision: 4, status: 'adopted', adopted: true }), offer({ status: 'changed', adopted_revision: 4 })] }) };
  const page = teamPage(reads.team, reads.teamLayers);
  assert.equal(page.needs.length, 2);
  assert.deepEqual(attentionCounts(settingsAttention(reads)).pages.team, { count: 2, worst: 'warn' });
  assert.deepEqual(settingsAttention({ team: { state: 'unlinked' } }), []);
  // The layers read failing leaves the link's own items.
  assert.equal(settingsAttention({ team: linkedStatus({ state: 'drift', problem: 'team.json changed' }) })[0].title, 'The link changed outside Crossing Guard');
});

test('the dialogs list what turns off', () => {
  const page = teamPage(linkedStatus(), layers({ adopted_bundles: [record()] }), options);
  assert.deepEqual(unadoptWords(page.bundles[0], { 'scope-watch': ['rate-limiter', 'storefront'] }), [['Rules', '7 stop applying on this device'],
    ['Scope watch', 'Turned off in rate-limiter and storefront'], ['Afterwards', 'Scope watch stays listed as “no longer shared by Harbor Street Labs”'],
    ['Adopting again', 'Does not turn it back on']]);
  assert.deepEqual(unadoptWords(page.bundles[0])[1], ['Scope watch', 'Turned off wherever it is on']);
  assert.deepEqual(unlinkWords(page).map(([label]) => label), ['This device’s key', 'Here', 'On the team server', 'What you adopted', 'Afterwards']);
  assert.equal(bundleTitle('repository:a/b'), 'Rules and agents for a/b');
  assert.equal(expiryWords('fail-open', true, false), 'Its rules stop applying');
  assert.equal(contentWords({ sync_content: 'mandated', repositories: ['a/b'] }), 'Required: tool inputs of sessions in a/b go to the team server');
  assert.equal(contentWords({ sync_content: 'consent' }), 'Asked for, one session at a time');
});

// Red-team Low 8: the approval address is the server's; only http and https open.
test('the approval address is a link only when it is http or https', () => {
  assert.equal(approvalHref('https://team.example/approve?code=1'), 'https://team.example/approve?code=1');
  assert.equal(approvalHref('http://127.0.0.1:8443/approve'), 'http://127.0.0.1:8443/approve');
  for (const address of ['javascript:alert(1)', 'data:text/html,<p>x</p>', 'file:///etc/passwd', 'team.example/approve', '', undefined]) {
    assert.equal(approvalHref(address), '', String(address));
  }
});

// Red-team Low 9: an unlink the server never heard of says so, in the daemon's words.
test('an unlink the team server could not be told of shows the daemon’s note', () => {
  const note = 'unlinked locally; the server could not be told, so it still lists this device until an admin revokes it';
  assert.equal(unlinkOutcome({ server_acknowledged: false, note }), note);
  assert.equal(unlinkOutcome({ server_acknowledged: true, note: 'unlinked; the server revoked this device\'s key and deleted nothing' }), '');
  assert.equal(unlinkOutcome(null), '');
  assert.notEqual(unlinkOutcome({ server_acknowledged: false }), '');
});

// Journey W-3: Details names the approver; the id is shown only when no name is known.
test('Details shows the approver’s name when the daemon has it', () => {
  const rows = status => teamDetails(status, layers({ pinned_org_key: {} }), options);
  const named = JSON.stringify(rows({ ...linkedStatus(), approved_by: 'usr_01ABC', approved_by_name: 'Maya Okafor' }));
  assert.ok(named.includes('Maya Okafor') && !named.includes('usr_01ABC'));
  assert.ok(JSON.stringify(rows({ ...linkedStatus(), approved_by: 'usr_01ABC' })).includes('usr_01ABC'));
});
