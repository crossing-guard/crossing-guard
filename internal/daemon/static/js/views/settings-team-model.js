// Settings → Team, as data (team rest-of-release plan §4.2, §14 Q4–Q7, Q31, Q33–Q36).
// Pure: no DOM, no fetch. teamPage turns the daemon's two reads (GET /api/team and
// GET /api/team/layers) into the page's five parts; the view draws what this says and
// the tests pin the rules of criterion 89. Everything a default view may show is in the
// page object; anything technical (the daemon's own error text, which may name an
// address) is under a `technical` key and drawn behind a disclosure.
import { count, teamMemoryText, deletionText } from './team-memory-text.js';

const BAD_REPORT_OUTCOMES = new Set(['error', 'rejected', 'revoked']);
const REFUSING_OUTCOMES = new Set(['rejected', 'revoked']);

// What a broken link is called, by the daemon's state: [title, what to do].
const LINK_PROBLEMS = Object.freeze({
  revoked: ['The team server no longer accepts this device', 'Its key was revoked. Ask an admin, or unlink and link again.'],
  drift: ['The link changed outside Crossing Guard', 'Nothing is sent until you unlink and link again.'],
  inconsistent: ['The team link is broken', 'Unlink and link again to reset it.'],
});

const CHIPS = Object.freeze({
  linked: { words: 'linked', tone: 'good' },
  unlinked: { words: 'unlinked', tone: 'neutral' },
  problem: { words: 'link problem', tone: 'bad' },
  waiting: { words: 'waiting for approval', tone: 'warn' },
  blocking: { words: 'rules expired — blocking', tone: 'bad' },
});

const DOCUMENT_STATES = Object.freeze({
  offered: 'Adopted when you adopt',
  adopted: 'Adopted',
  cant_be_used: 'Can’t be used',
  not_applied: 'Not applied by this version',
  id_in_use: 'Not adopted — your agent uses this id',
});

// formats words dates in the reader's own locale and zone; the tests pin both.
function formats({ locale, timeZone } = {}) {
  const valid = iso => { const date = new Date(iso); return isNaN(date) ? null : date; };
  const day = { day: 'numeric', month: 'short', year: 'numeric', timeZone };
  return {
    day: iso => valid(iso)?.toLocaleDateString(locale, day) || '',
    when: iso => valid(iso)?.toLocaleString(locale, { ...day, hour: '2-digit', minute: '2-digit' }) || '',
  };
}

const list = names => names.length < 2 ? names.join('') : names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1];
const organizationName = status => String(status?.organization?.name || '') || 'The team';
const repositoryOf = scope => String(scope || '').startsWith('repository:') ? String(scope).slice('repository:'.length) : '';

// bundleTitle names a bundle by what it is to the person, never by its id.
export function bundleTitle(scope) {
  const repository = repositoryOf(scope);
  return repository ? 'Rules and agents for ' + repository : 'Organization rules and agents';
}

// expiryWords says what happens when a bundle expires, for what the bundle carries.
export function expiryWords(mode, rules = true, agents = true) {
  if (mode === 'fail-closed') return 'Governed tool calls are blocked until it is refreshed';
  if (rules && agents) return 'Its rules stop applying; its agents are held';
  return rules ? 'Its rules stop applying' : 'Its agents are held';
}

// contentWords states a bundle's content policy. It takes the signed object, or the
// one-line JSON the daemon spells it as in a changed field, or nothing.
export function contentWords(policy) {
  let value = policy;
  if (typeof policy === 'string') {
    try { value = policy ? JSON.parse(policy) : null; } catch { return policy; }
  }
  const mode = String(value?.sync_content || 'off');
  const repositories = value?.repositories || [];
  if (mode === 'mandated') {
    return repositories.length ? 'Required: tool inputs of sessions in ' + list(repositories) + ' go to the team server'
      : 'Required: tool inputs of every session go to the team server';
  }
  return mode === 'consent' ? 'Asked for, one session at a time' : 'Not asked for';
}

function sharesWords(rules, agents) {
  const parts = [rules ? count(rules, 'rule', 'rules') : '', agents ? count(agents, 'agent', 'agents') : ''].filter(Boolean);
  return parts.join(' and ') || 'nothing this version applies';
}

function offerParts(offer) {
  const documents = offer.documents || [];
  const profiles = documents.filter(item => item.kind === 'profile');
  return { rules: (offer.rules || []).length, agents: profiles.filter(item => item.state !== 'id_in_use'),
    collisions: profiles.filter(item => item.state === 'id_in_use') };
}

const ruleLine = rule => [rule.id, rule.action, rule.intent].filter(Boolean).join(' — ');
const agentName = item => String(item.profile_name || item.name || item.profile_id || 'An agent');

function ruleChangeWords(changes) {
  return [changes.added.length ? changes.added.length + ' added' : '', changes.changed.length ? changes.changed.length + ' changed' : '',
    changes.removed.length ? changes.removed.length + ' removed' : ''].filter(Boolean).join(', ');
}

// ruleDiff is the rule block of an offer: every rule when nothing is adopted for the
// scope, the difference from the adopted rulebook when something is.
function ruleDiff(offer) {
  const rules = offer.rules || [], changes = offer.rule_changes;
  if (!changes) {
    if (offer.status !== 'offered' || !rules.length) return null;
    return { head: 'Rules: ' + rules.length, lines: rules.map(rule => ({ op: 'add', text: ruleLine(rule) })) };
  }
  const byId = new Map(rules.map(rule => [rule.id, rule]));
  const side = (id, action, predicate) => [id, action, predicate].filter(Boolean).join(' — ');
  const lines = [
    ...changes.added.map(id => ({ op: 'add', text: ruleLine(byId.get(id) || { id }) })),
    ...changes.changed.flatMap(item => [
      { op: 'remove', text: side(item.id, item.before_action, item.before_predicate) },
      { op: 'add', text: side(item.id, item.after_action, item.after_predicate) }]),
    ...changes.removed.map(id => ({ op: 'remove', text: id })),
  ];
  return { head: 'Rules: ' + ruleChangeWords(changes), lines };
}

// profileDiff is one shared agent in an offer: its text, or for a changed one the
// lines that differ.
function profileDiff(item) {
  const name = agentName(item);
  if (item.change === 'update' && (item.text_diff || []).length) {
    const lines = item.text_diff.filter(line => line.op !== 'keep');
    const added = lines.filter(line => line.op === 'add').length;
    return { head: name + ': instructions, ' + added + ' added, ' + (lines.length - added) + ' removed', lines };
  }
  const words = item.state === 'id_in_use' ? 'not adopted — your agent uses this id'
    : item.change === 'unchanged' ? 'the same as the agent here' : 'new agent';
  return { head: name + ': ' + words, text: String(item.text || '') };
}

const FIELD_WORDS = Object.freeze({
  failure_mode: ['If it expires', value => expiryWords(value)],
  content_policy: ['Session content', value => contentWords(value)],
  schema_version: ['Bundle format', value => String(value || 'not recorded')],
  key_id: ['Signing key', (value, side) => side === 'from' ? 'The key trusted before' : 'The key this device trusts now'],
});

// fieldDiffs puts each changed signed field on its own labelled block.
function fieldDiffs(offer) {
  return (offer.changes || []).map(change => {
    const [head, words] = FIELD_WORDS[change.field] || [String(change.label || change.field), value => String(value)];
    return { head, lines: [{ op: 'remove', text: words(change.from, 'from') }, { op: 'add', text: words(change.to, 'to') }] };
  });
}

function documentRows(offer) {
  return (offer.documents || []).map(item => {
    const name = item.kind === 'rulebook' ? count((offer.rules || []).length, 'rule', 'rules')
      : item.kind === 'profile' ? agentName(item) : item.kind === 'detectors' ? 'Detectors' : String(item.name || item.kind);
    return [name, DOCUMENT_STATES[item.state] || String(item.state || '')];
  });
}

function offerFacts(offer, parts, fmt) {
  const facts = [];
  if (offer.rule_changes) facts.push(['Rules', ruleChangeWords(offer.rule_changes)]);
  else if (parts.rules && offer.status === 'offered') facts.push(['Rules', String(parts.rules)]);
  const added = offer.status === 'offered' ? parts.agents : parts.agents.filter(item => item.change === 'new');
  if (added.length) facts.push([offer.status === 'offered' ? 'Agents' : 'New agents', list(added.map(agentName)) + ', off until you turn ' + (added.length === 1 ? 'it' : 'them') + ' on']);
  for (const item of parts.collisions) facts.push(['Not adopted', agentName(item) + ': your own agent already uses that id']);
  if (offer.status !== 'changed') {
    facts.push(['Session content', contentWords(offer.content_policy)]);
    facts.push(['If it expires', expiryWords(offer.failure_mode, parts.rules > 0, parts.agents.length > 0)]);
  }
  facts.push(['Valid until', fmt.day(offer.expires_at)]);
  return facts;
}

const OFFER_WORDS = Object.freeze({
  changed: offer => ['Revision ' + offer.revision + ' changes what you adopted', 'Still on revision ' + offer.adopted_revision + ' until you adopt.', 'Adopt revision ' + offer.revision],
  partial: offer => ['Adopting revision ' + offer.revision + ' did not finish', 'Part of it is in force. Finish adopting to apply the rest.', 'Finish adopting'],
});

// offerItem is one bundle that asks: offered, changed, or partly adopted. Adopt acts
// from the card that shows the difference, so the difference is part of the item.
function offerItem(offer, organization, fmt) {
  const parts = offerParts(offer);
  const repository = repositoryOf(offer.scope);
  const [title, sub, label] = (OFFER_WORDS[offer.status] || (() => [
    organization + ' shares ' + sharesWords(parts.rules, parts.agents.length) + (repository ? ' for ' + repository : ''),
    'Nothing applies until you adopt.', 'Adopt']))(offer);
  const profiles = (offer.documents || []).filter(item => item.kind === 'profile' && (offer.status !== 'changed' || item.change !== 'unchanged'));
  return { kind: 'offer', severity: 'warn', title, sub, facts: offerFacts(offer, parts, fmt),
    diffs: [...fieldDiffs(offer), ruleDiff(offer), ...profiles.map(profileDiff)].filter(Boolean),
    documents: documentRows(offer), inline: offer.status === 'changed',
    actions: [{ kind: 'adopt', label, primary: true, scope: offer.scope, bundle: offer.id, token: offer.state_token }] };
}

function collisionItems(offer) {
  return offerParts(offer).collisions.map(item => ({ kind: 'collision', severity: 'info', title: agentName(item) + ' was not adopted',
    sub: 'Your own agent already uses its id. Yours is unchanged.',
    actions: [{ kind: 'agent', label: 'Your ' + agentName(item) + ' ›', agent: String(item.profile_id || '') }] }));
}

function unusableItem(bundle, organization) {
  const still = bundle.still_on_revision ? 'Still on revision ' + bundle.still_on_revision + '.' : 'Nothing from it is adopted.';
  return { kind: 'unusable', severity: 'info', title: 'Revision ' + bundle.revision + ' can’t be used',
    sub: still + ' Only ' + organization + '’s lead can fix it.', technical: String(bundle.reason || ''), actions: [] };
}

// keyItem is the one place a fingerprint leaves Details: the person must compare the
// presented key with the Policy page before trusting it.
function keyItem(mismatch, organization) {
  const bundles = (mismatch.bundles || []).length;
  return { kind: 'key', severity: 'bad', title: organization + ' has a new signing key',
    sub: 'What it signs is not offered until you trust it. What you adopted stays in force.',
    prints: [['Trusted now', String(mismatch.pinned_fingerprint || '')], ['New', String(mismatch.presented_fingerprint || '')]],
    facts: [['Signs', count(bundles, 'bundle', 'bundles') + ' not offered yet']],
    actions: [{ kind: 'trust', label: 'Trust new key…', primary: true, pinned: String(mismatch.pinned_fingerprint || ''), presented: String(mismatch.presented_fingerprint || '') }] };
}

// expiredItem says what an expired bundle holds (fail-open) or blocks (fail-closed).
function expiredItem(bundle, sending) {
  const facts = [];
  if (bundle.blocking) facts.push(['Blocked', bundle.repository ? 'Every governed tool call in ' + bundle.repository : 'Every governed tool call on this device']);
  if (bundle.ruleCount && !bundle.blocking) facts.push(['Rules', bundle.ruleCount + ', not applied since ' + bundle.expiresOn]);
  for (const agent of bundle.agents) facts.push([agent.name, 'Held; it starts no run']);
  if (sending.failing && sending.since) facts.push(['Team server', 'Not reached since ' + sending.since]);
  const refresh = bundle.linked ? 'The next refresh lifts it with no action from you.'
    : 'This device is not linked to ' + bundle.organization + ', so nothing refreshes it.';
  if (bundle.blocking) {
    return { kind: 'expired', severity: 'bad', title: bundle.organization + '’s rules expired on ' + bundle.expiresOn + ' and are blocking',
      sub: refresh + ' Un-adopt lifts it at once.', facts,
      actions: [{ kind: 'unadopt', label: 'Un-adopt…', danger: true, bundle }] };
  }
  return { kind: 'expired', severity: 'bad', title: sharesTitle(bundle) + ' from ' + bundle.organization + ' expired on ' + bundle.expiresOn,
    sub: bundle.linked ? 'They come back by themselves when this device reaches the team server.' : refresh, facts,
    actions: bundle.agents.map(agent => ({ kind: 'agent', label: agent.name + ' ›', agent: agent.id })) };
}

function sharesTitle(bundle) {
  if (bundle.ruleCount && bundle.agents.length) return 'Rules and agents';
  return bundle.ruleCount ? 'Rules' : 'Agents';
}

// sendingState reads whether what this device sends is getting through.
function sendingState(status, fmt) {
  const report = status.report || {}, outbox = status.outbox || {};
  const failing = BAD_REPORT_OUTCOMES.has(report.outcome) || outbox.outcome === 'error';
  const refusing = REFUSING_OUTCOMES.has(report.outcome) || status.state === 'revoked';
  return { failing, refusing, since: failing ? fmt.day(outbox.oldest_at || report.last_at) : '',
    technical: failing ? String(outbox.error || report.error || report.outcome || '') : '' };
}

function linkItems(status, organization, sending) {
  const items = [];
  const [title, sub] = LINK_PROBLEMS[status.state] || [];
  if (title) {
    items.push({ kind: 'link', severity: 'bad', title, sub, technical: String(status.problem || ''), actions: [] });
  } else if (sending.failing) {
    items.push({ kind: 'link', severity: 'bad', title: sending.refusing ? organization + '’s server is refusing what this device sends' : organization + '’s server is not answering',
      sub: sending.refusing ? 'Nothing is lost here. Ask an admin why.' : 'What waits goes out when it answers.', technical: sending.technical, actions: [] });
  }
  const outbox = status.outbox || {};
  if (outbox.over_high_water && !sending.failing) {
    items.push({ kind: 'waiting', severity: 'warn', title: Number(outbox.pending || 0).toLocaleString() + ' are waiting to send',
      sub: 'More is waiting than this device usually holds. It goes out a batch at a time.', actions: [] });
  }
  const conflicts = Object.values(outbox.conflicts || {}).reduce((sum, n) => sum + Number(n || 0), 0);
  if (conflicts) {
    items.push({ kind: 'conflict', severity: 'warn', title: count(conflicts, 'record here differs', 'records here differ') + ' from what the team holds',
      sub: 'The team’s copy was kept. Memory shows each one.', actions: [{ kind: 'view', label: 'Memory ›', view: 'memory' }] });
  }
  return items;
}

// bundleView is one adopted-bundle record in the person's terms.
function bundleView(record, offers, fmt) {
  const agents = (record.agents || []).map(item => ({ id: String(item.profile_id || ''), name: String(item.name || item.profile_id || '') }));
  const ruleCount = Number(record.rule_count || 0);
  const offer = offers.find(item => item.id === record.bundle_id);
  const organization = String(record.organization_name || '') || 'the team';
  const expiresOn = fmt.day(record.expires_at);
  const facts = [['Adopted', fmt.day(record.adopted_at)], [record.expired ? 'Expired' : 'Valid until', expiresOn],
    ['If it expires', expiryWords(record.failure_mode, ruleCount > 0, agents.length > 0)], ['Session content', contentWords(record.content_policy)]];
  const documents = offer ? documentRows(offer)
    : [...(ruleCount ? [[count(ruleCount, 'rule', 'rules'), 'Adopted']] : []), ...agents.map(agent => [agent.name, 'Adopted'])];
  return { organizationId: String(record.organization_id || ''), organization, scope: String(record.scope || ''), repository: repositoryOf(record.scope),
    title: bundleTitle(record.scope), revision: Number(record.revision || 0), ruleCount, agents, expiresOn,
    expired: Boolean(record.expired), blocking: Boolean(record.blocking), linked: Boolean(record.linked), facts, documents,
    collisions: offer ? offerParts(offer).collisions.map(agentName) : [] };
}

// needsYou lists what needs the person, worst first. It is empty when nothing does,
// and the block is then not drawn at all.
function needsYou(status, bundles, layers, mode, fmt) {
  const organization = organizationName(status);
  const sending = sendingState(status, fmt);
  const items = [];
  if (mode === 'linked') {
    items.push(...linkItems(status, organization, sending));
    for (const mismatch of layers.key_mismatches || []) items.push(keyItem(mismatch, organization));
    for (const offer of layers.available || []) {
      if (offer.status === 'adopted') items.push(...collisionItems(offer));
      else items.push(offerItem(offer, organization, fmt));
    }
    for (const bundle of layers.unusable || []) items.push(unusableItem(bundle, organization));
  }
  for (const bundle of bundles) if (bundle.expired) items.push(expiredItem(bundle, sending));
  const order = { bad: 0, warn: 1, info: 2 };
  return items.sort((a, b) => order[a.severity] - order[b.severity]);
}

function rulesRow(linked, offered, fmt) {
  if (!linked.length) return { label: 'Rules', value: offered ? 'None adopted' : 'None shared yet', subs: [] };
  const total = linked.reduce((sum, bundle) => sum + bundle.ruleCount, 0);
  const live = linked.filter(bundle => !bundle.expired);
  const inForce = live.reduce((sum, bundle) => sum + bundle.ruleCount, 0);
  const value = linked.some(bundle => bundle.blocking) ? total + ', expired and blocking'
    : live.length ? (inForce ? inForce + ' in force' : 'None in force') : total + ', not applied';
  const subs = linked.map(bundle => (linked.length > 1 ? (bundle.repository || 'Organization') + ': revision ' : 'Revision ') + bundle.revision
    + (bundle.expired ? ' expired ' : ', valid until ') + bundle.expiresOn);
  return { label: 'Rules', value, subs };
}

function agentsRow(linked, offered) {
  const action = { kind: 'settings', label: 'Agents ›', page: 'agents' };
  const names = linked.flatMap(bundle => bundle.agents.map(agent => agent.name));
  const collisions = linked.flatMap(bundle => bundle.collisions);
  const subs = collisions.length ? [collisions.length + ' not adopted: ' + list(collisions) + ', your own agent uses that id'] : [];
  if (!names.length) return { label: 'Shared agents', value: offered || linked.length ? 'None adopted' : 'None shared yet', subs, action };
  const held = linked.every(bundle => bundle.expired);
  return { label: 'Shared agents', value: held ? list(names) + ', held' : names.length + ' adopted: ' + list(names), subs, action };
}

function teamRows(status, linked, layers, fmt) {
  const offered = (layers.available || []).length > 0;
  const pulled = Number(status.memory?.pulled || 0);
  return [rulesRow(linked, offered, fmt), agentsRow(linked, offered),
    { label: 'Shared memory', value: pulled ? count(pulled, 'record', 'records') : 'No records', subs: [], action: { kind: 'view', label: 'Memory ›', view: 'memory' } }];
}

// waitingRow is drawn only when something waits or fails: how many, since when, why,
// and what happens next. There is no meter here; that is in Details.
function waitingRow(status, fmt) {
  const outbox = status.outbox;
  if (!outbox) return null;
  const sending = sendingState(status, fmt), pending = Number(outbox.pending || 0);
  if (!pending && !sending.failing) return null;
  const since = outbox.oldest_at ? ' since ' + fmt.when(outbox.oldest_at) : '';
  const why = status.state === 'revoked' ? 'Refused: this device’s key is revoked'
    : sending.refusing ? 'The team server is refusing them'
      : sending.failing ? 'The team server is not answering. They go out when it does.' : 'They go out with the next send.';
  return { label: 'Waiting to send', value: pending ? pending.toLocaleString() + since : 'Nothing', subs: [pending ? why : 'The last send failed. The next one tries again.'] };
}

function contentRow(status, organization) {
  const content = status.content || {};
  const repositories = content.mandate?.repositories || [];
  if (content.mandate) {
    return { label: 'Session content', value: repositories.length ? 'On for sessions in ' + list(repositories) : 'On for every session',
      subs: ['Required by ' + organization + '’s rules'] };
  }
  const opted = (content.opted_in || []).length;
  return { label: 'Session content', value: opted ? 'On for ' + count(opted, 'session', 'sessions') : 'Off', subs: ['Turned on per session, from the session'] };
}

function deviceRows(status, mode, fmt) {
  if (mode !== 'linked') return [{ label: 'Leaves this machine', value: mode === 'pending' ? 'Nothing until it is approved' : 'Nothing', subs: [] }];
  const memory = status.memory || {}, shared = Number(memory.shared || 0), more = Number(memory.share_candidates || 0);
  const rows = [{ label: 'Session activity', value: 'On', subs: ['What ran and what was decided'] }, contentRow(status, organizationName(status)),
    { label: 'Memory shared from here', value: shared ? count(shared, 'record', 'records') : 'None', subs: [],
      action: more > 0 ? { kind: 'share', label: 'Share ' + more + ' more…', count: more } : null }];
  const waiting = waitingRow(status, fmt);
  if (waiting) rows.push(waiting);
  return rows;
}

// leftoverGroups lists what this device still holds from an organization it is not
// linked to, under that organization (Q31, criterion 73).
function leftoverGroups(bundles) {
  const groups = new Map();
  for (const bundle of bundles.filter(item => !item.linked)) {
    const key = bundle.organizationId + '\u0000' + bundle.organization;
    const group = groups.get(key) || { organization: bundle.organization, bundles: [] };
    group.bundles.push(bundle);
    groups.set(key, group);
  }
  return [...groups.values()].map(group => ({ organization: group.organization, rows: group.bundles.flatMap(leftoverRows) }));
}

function leftoverRows(bundle) {
  const label = bundle.repository ? bundle.repository + ' rules' : 'Organization rules';
  const state = bundle.blocking ? 'expired and blocking' : bundle.expired ? 'not applied' : 'in force';
  const after = bundle.expired ? 'Expired ' + bundle.expiresOn : 'Valid until ' + bundle.expiresOn + ', then ' + (bundle.ruleCount ? 'they stop applying' : 'its agents are held');
  const rows = [{ label, value: bundle.ruleCount ? bundle.ruleCount + ' ' + state : 'No rules', subs: [after], action: { kind: 'unadopt', label: 'Un-adopt…', danger: true, bundle } }];
  for (const agent of bundle.agents) {
    rows.push({ label: agent.name, value: 'Shared agent', subs: [bundle.expired ? 'Held since ' + bundle.expiresOn : 'Held after ' + bundle.expiresOn],
      action: { kind: 'agent', label: 'Agents ›', agent: agent.id } });
  }
  return rows;
}

function chipFor(mode, status, bundles, needs) {
  if (mode === 'pending') return CHIPS.waiting;
  if (bundles.some(bundle => bundle.blocking)) return CHIPS.blocking;
  if (mode === 'unlinked') return CHIPS.unlinked;
  return needs.some(item => item.kind === 'link') ? CHIPS.problem : CHIPS.linked;
}

function pendingCard(pending, fmt) {
  return { title: 'Approve this device in the team console', rows: [['Open', String(pending.verification_url || '')], ['Code', String(pending.user_code || '')],
    ['This device’s key', String(pending.fingerprint || '')], ['Device name', String(pending.name || '')], ['Code expires', fmt.when(pending.expires_at)]] };
}

function headingFor(mode, status) {
  if (mode === 'pending') {
    let host = String(status.pending.server || '');
    try { host = new URL(host).host; } catch { /* the daemon's own spelling */ }
    return 'Linking to ' + host;
  }
  return mode === 'unlinked' ? 'Not linked to a team' : 'Linked to ' + (String(status.organization?.name || '') || 'a team');
}

// teamPage is the whole page as data. `needs` empty means no needs-you block.
export function teamPage(status, layers, options = {}) {
  const read = status || {}, offer = layers || {}, fmt = formats(options);
  const mode = read.pending ? 'pending' : read.state === 'unlinked' || !read.state ? 'unlinked' : 'linked';
  const bundles = (offer.adopted_bundles || []).map(record => bundleView(record, mode === 'linked' ? offer.available || [] : [], fmt));
  const linked = bundles.filter(bundle => bundle.linked);
  const needs = needsYou(read, bundles, offer, mode, fmt);
  return { mode, heading: headingFor(mode, read), chip: chipFor(mode, read, bundles, needs), needs,
    problem: mode === 'unlinked' ? String(read.problem || '') : '',
    pending: mode === 'pending' ? pendingCard(read.pending, fmt) : null,
    team: mode === 'linked' ? teamRows(read, linked, offer, fmt) : [], bundles: linked,
    leftovers: leftoverGroups(bundles), device: deviceRows(read, mode, fmt) };
}

// teamAttention is what the Settings sidebar and the Overview count for Team: exactly
// the needs-you block, one counted item each, so the count is absent when it is empty.
export function teamAttention(status, layers) {
  if (!status) return [];
  return teamPage(status, layers).needs.map(item => ({ severity: item.severity === 'bad' ? 'bad' : 'warn', page: 'team', target: '',
    title: item.title, facts: [item.sub].filter(Boolean) }));
}

// unadoptWords lists what turns off, for the Un-adopt dialog. `places` maps an agent id
// to the names of the places it is on in, when the roster could be read.
export function unadoptWords(bundle, places = {}) {
  const rows = [];
  if (bundle.ruleCount) rows.push(['Rules', count(bundle.ruleCount, 'stops', 'stop') + ' applying on this device']);
  if (bundle.blocking) rows.push(['Blocked tool calls', 'Unblocked at once']);
  for (const agent of bundle.agents) {
    const on = places[agent.id];
    rows.push([agent.name, on ? (on.length ? 'Turned off in ' + list(on) : 'Not on anywhere') : 'Turned off wherever it is on']);
  }
  if (bundle.agents.length) {
    const names = list(bundle.agents.map(agent => agent.name));
    rows.push(['Afterwards', names + (bundle.agents.length === 1 ? ' stays' : ' stay') + ' listed as “no longer shared by ' + bundle.organization + '”']);
    rows.push(['Adopting again', 'Does not turn ' + (bundle.agents.length === 1 ? 'it' : 'them') + ' back on']);
  }
  return rows;
}

// approvalHref is the address the approval link may open: the server supplies it, so
// only an http or https address becomes a link. Anything else ('' is returned) is
// shown as plain text and opens nothing.
export function approvalHref(address) {
  try {
    const url = new URL(String(address || ''));
    return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : '';
  } catch { return ''; }
}

// unlinkOutcome is what the person is told after an unlink the team server did not
// hear of: the daemon's own note. '' when the server acknowledged it.
export function unlinkOutcome(result) {
  if (!result || result.server_acknowledged !== false) return '';
  return String(result.note || 'Unlinked here; the team server could not be told.');
}

// unlinkWords lists what unlinking does and does not do.
export function unlinkWords(page) {
  const rows = [['This device’s key', 'Revoked on the team server, if it can be reached'], ['Here', 'Nothing is deleted'], ['On the team server', 'Nothing is deleted']];
  if (page.bundles.length) rows.push(['What you adopted', 'Stays in force until it expires']);
  rows.push(['Afterwards', 'Nothing leaves this device']);
  return rows;
}

// cadenceWords spells a daemon duration ("15m0s", "30s", "1h30m0s") for a person.
export function cadenceWords(duration) {
  const parts = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(String(duration || ''));
  if (!parts || !parts[0]) return String(duration || '');
  const [, hours, minutes, seconds] = parts.map(value => Number(value || 0));
  return [hours ? hours + ' h' : '', minutes ? minutes + ' min' : '', seconds ? seconds + ' s' : ''].filter(Boolean).join(' ') || '0 s';
}

const byCode = codes => Object.entries(codes || {}).map(([code, n]) => n + ' ' + code).join(', ');
const outcomeLine = part => [part.outcome, part.error].filter(Boolean).join(': ');

function linkDetailRows(status, layers, fmt) {
  const device = status.device || {}, pin = layers.pinned_org_key || {};
  return [
    { label: 'Team server', lines: [status.server] },
    { label: 'Device name', lines: [device.name] },
    { label: 'Platform', lines: [device.platform] },
    { label: 'Device id', lines: [device.id], mono: true },
    { label: 'Device key', lines: [device.fingerprint], mono: true },
    { label: 'Linked', lines: [fmt.when(status.linked_at)] },
    { label: 'Approved by', lines: [status.approved_by_name || status.approved_by] },
    { label: 'Organization key', lines: [pin.fingerprint], mono: true },
    { label: 'Key id', lines: [pin.key_id], mono: true },
    { label: 'Key trusted', lines: [fmt.when(pin.pinned_at)] },
  ];
}

function sendDetailRows(status, fmt) {
  const report = status.report || {}, outbox = status.outbox || {}, pull = status.memory?.pull || {};
  const rows = [
    { label: 'Last report', lines: [report.last_at ? fmt.when(report.last_at) : 'Not yet', outcomeLine(report), report.next_at ? 'Next ' + fmt.when(report.next_at) : ''] },
    { label: 'Reports', lines: [report.interval ? 'Every ' + cadenceWords(report.interval) : ''] },
    { label: 'Last pull', lines: [pull.last_at ? fmt.when(pull.last_at) : status.memory ? 'Not yet' : '', outcomeLine(pull)] },
    { label: 'Pulls', lines: [pull.interval ? 'Every ' + cadenceWords(pull.interval) : ''] },
  ];
  if (!status.outbox) return rows;
  const pending = Number(outbox.pending || 0), mark = Number(outbox.high_water || 0);
  rows.push(
    { label: 'Last send', lines: [outbox.last_at ? fmt.when(outbox.last_at) : 'Not yet', outcomeLine(outbox), Number(outbox.accepted || 0).toLocaleString() + ' accepted by the server so far'] },
    { label: 'Sends', lines: ['Every ' + cadenceWords(outbox.interval) + ', up to ' + outbox.batch + ' at a time'] },
    { label: 'Waiting to send', lines: [pending.toLocaleString() + ' waiting', mark ? 'High-water mark ' + mark.toLocaleString() : '', outbox.over_high_water ? 'Over the high-water mark' : '',
      outbox.oldest_at ? 'Oldest ' + fmt.when(outbox.oldest_at) : ''], meter: mark ? { pending, mark, over: Boolean(outbox.over_high_water) } : null },
    { label: 'Waiting, by kind', lines: Object.entries(outbox.by_kind || {}).map(([kind, n]) => n + ' ' + kind) },
    { label: 'Kinds the server does not accept', lines: (outbox.parked || []).map(item => item.kind + ': ' + item.reason + (item.next_probe_at ? ', tried again ' + fmt.when(item.next_probe_at) : '')) },
    { label: 'Refused by the server', lines: Object.entries(outbox.dead_letter || {}).map(([kind, codes]) => kind + ': ' + byCode(codes)) },
    { label: 'Not sent from this device', lines: Object.entries(outbox.refused || {}).map(([kind, codes]) => kind + ': ' + byCode(codes)) },
    { label: 'Conflicts', lines: Object.entries(outbox.conflicts || {}).map(([kind, n]) => n + ' ' + kind + ' conflicted with what the server holds') });
  return rows;
}

function contentDetailRows(status, fmt) {
  const content = status.content || {};
  const mandate = content.mandate ? ['Required by bundle ' + content.mandate.bundle_id,
    (content.mandate.repositories || []).length ? 'For repositories ' + list(content.mandate.repositories) : 'For every session'] : [];
  return [
    { label: 'What is sent', lines: status.sends || [] },
    { label: 'Session content', lines: [...mandate, ...(content.opted_in || []).map(item => item.session + ', on since ' + fmt.when(item.since)),
      content.session_cap ? 'At most ' + Number(content.session_cap).toLocaleString() + ' bytes for a session' : '', content.retention] },
  ];
}

function bundleDetailRows(layers) {
  const records = layers.adopted_bundles || [];
  return [
    { label: 'Adopted bundles', lines: records.map(record => bundleTitle(record.scope) + ' from ' + (record.organization_name || record.organization_id) + ': '
      + record.bundle_id + ', signed by ' + record.key_id + ', ' + record.signed_digest), mono: true },
    { label: 'Not offered', lines: layers.reasons || [] },
  ];
}

// teamDetails is the collapsed Details part: every identifier, fingerprint, address
// and cadence, and (Q34) every fact the earlier page showed that the default view no
// longer does. A row with no lines is not drawn. `deletions` is the answer of
// GET /api/team/memory/deletions, or null when it was not read.
export function teamDetails(status, layers, deletions = null, options = {}) {
  const read = status || {}, offer = layers || {}, fmt = formats(options);
  const linked = !read.pending && read.state && read.state !== 'unlinked';
  const rows = linked ? [...linkDetailRows(read, offer, fmt), ...sendDetailRows(read, fmt), ...contentDetailRows(read, fmt)]
    : [{ label: 'Device id', lines: [read.device?.id], mono: true }];
  if (linked && read.memory) rows.push({ label: 'Memory', lines: teamMemoryText(read.memory) });
  if (deletions && (deletions.deletions || []).length) {
    rows.push({ label: 'Memory deletions', lines: [...deletions.deletions.map(deletionText), deletions.retention] });
  }
  rows.push(...bundleDetailRows(offer));
  const kept = rows.map(row => ({ ...row, lines: (row.lines || []).map(line => String(line || '')).filter(Boolean) })).filter(row => row.lines.length);
  return { rows: kept, document: linked && read.report?.document ? JSON.stringify(read.report.document, null, 2) : '' };
}

export { formats as teamFormats };
