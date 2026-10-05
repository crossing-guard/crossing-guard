import test from 'node:test';
import assert from 'node:assert/strict';
import {
  routeFacts, needsRoute, fallbackFacts, routeChoices, routeFamilyFor, localityWords, outageWords, outageDetail, outageReported,
  originView, collisionView, holdWords, refusalWords, placeStatus, stoppedWords, shareView,
  NO_ROUTE_YET, ROUTE_FAMILY_MANAGED, ROUTE_FAMILY_REVIEW,
} from './route-model.js';
import { indexRow, indexRows, placeDifferences, attentionWords, deliveryWords } from './roster-model.js';

// A fixture route whose model id and endpoint are known strings (criterion 56).
const MODEL_ID = 'zz-fixture-model-9000';
const ENDPOINT = 'http://127.0.0.1:45999';
const RUNTIME_ID = 'rt-fixture';

const managedPlace = (overrides = {}) => ({
  place_id: 'agent-x', lane: 'managed', state: 'enabled', repository: 'repo-one', project_root: '/work/repo-one',
  runtime: RUNTIME_ID, model: MODEL_ID, thinking_effort: { kind: 'level', value: 'high' }, mode: 'read-only',
  route_id: 'rte_fast', route_name: 'Fast reader', route_local: false, scope_runtime: RUNTIME_ID,
  routes: [{ route_id: 'rte_second', runtime: RUNTIME_ID, model: MODEL_ID, mode: 'read-only' }],
  fallback_routes: [{ route_id: 'rte_second', route_name: 'Second choice', route_local: false, mode: 'read-only' }],
  granted_authority: [], declared_tags: [], limits: {}, updated_at: 10, ...overrides,
});

const reviewPlace = (overrides = {}) => ({
  place_id: 'review', lane: 'review', state: 'enabled', model: MODEL_ID, endpoint: ENDPOINT, local: true,
  route_id: 'rte_careful', route_name: 'Careful reviewer', route_local: true, ...overrides,
});

const routes = [
  { route_id: 'rte_fast', name: 'Fast reader', family: ROUTE_FAMILY_MANAGED, local: false,
    fields: { runtime: RUNTIME_ID, model: MODEL_ID, thinking_effort: { kind: 'level', value: 'high' } } },
  { route_id: 'rte_careful', name: 'Careful reviewer', family: ROUTE_FAMILY_REVIEW, local: true,
    fields: { endpoint: ENDPOINT, model: MODEL_ID } },
];

test('an agent surface shows a route by name and locality only', () => {
  assert.deepEqual(routeFacts(managedPlace()), { name: 'Fast reader', locality: 'leaves this machine', problem: '', short: 'Fast reader' });
  assert.deepEqual(routeFacts(reviewPlace()), { name: 'Careful reviewer', locality: 'on this machine', problem: '', short: 'Careful reviewer' });
  assert.equal(localityWords(true), 'on this machine');
  assert.equal(routeFamilyFor('review'), ROUTE_FAMILY_REVIEW);
  assert.equal(routeFamilyFor('managed'), ROUTE_FAMILY_MANAGED);
});

test('a missing route and a place with no route yet say so in the decided words', () => {
  const missing = routeFacts(managedPlace({ route_problem: 'route_missing', route_name: '' }));
  assert.equal(missing.name, 'Missing — this place starts no run');
  assert.equal(missing.short, 'Model route missing');
  assert.equal(routeFacts(reviewPlace({ route_problem: 'route_missing' })).name, 'Missing — asks go to you');
  const unrouted = routeFacts(managedPlace({ route_id: '', route_name: '', route_problem: 'migration_failed' }));
  assert.equal(unrouted.name, 'Not on a route yet · runs as it did before');
  assert.equal(unrouted.name, NO_ROUTE_YET);
  assert.equal(unrouted.locality, '');
  assert.equal(needsRoute(managedPlace({ route_problem: 'route_missing' })), true);
  assert.equal(needsRoute(managedPlace()), false);
});

test('the fallback chain is named by route, a missing entry is said', () => {
  assert.deepEqual(fallbackFacts(managedPlace()), [{ id: 'rte_second', name: 'Second choice', locality: 'leaves this machine', mode: 'read-only' }]);
  assert.equal(fallbackFacts({ fallback_routes: [{ route_id: 'rte_gone', missing: true }] })[0].name, 'A missing route (passed over)');
  assert.equal(fallbackFacts({ fallback_routes: [{ mode: 'plan' }] })[0].name, 'Not on a route yet');
  assert.deepEqual(fallbackFacts({}), []);
});

test('route choices list one family by name with the modes a place may pick', () => {
  const modesFor = route => (route.fields.runtime === RUNTIME_ID ? [{ id: 'read-only', label: 'Read only' }, { id: 'plan', label: 'Plan' }] : []);
  const managed = routeChoices(routes, ROUTE_FAMILY_MANAGED, modesFor);
  assert.deepEqual(managed, [{ id: 'rte_fast', name: 'Fast reader', locality: 'leaves this machine',
    modes: [{ id: 'read-only', label: 'Read only' }, { id: 'plan', label: 'Plan' }] }]);
  assert.deepEqual(routeChoices(routes, ROUTE_FAMILY_REVIEW).map(choice => choice.name), ['Careful reviewer']);
  assert.deepEqual(routeChoices([], ROUTE_FAMILY_MANAGED), []);
});

test('an outage leads with the route’s name and names no model', () => {
  assert.equal(outageWords({ runtime: RUNTIME_ID, model: MODEL_ID, route_names: ['Fast reader'], parked: 1 }, '3:04 PM'),
    'Fast reader is not answering since 3:04 PM. 1 run is parked.');
  assert.equal(outageWords({ runtime: RUNTIME_ID, model: MODEL_ID, route_names: ['A', 'B'], parked: 0 }),
    'A, B are not answering. 0 runs are parked.');
  assert.equal(outageWords({ runtime: RUNTIME_ID, model: MODEL_ID, route_names: [], parked: 2 }),
    'A model your agents use is not answering. 2 runs are parked.');
  assert.equal(outageDetail({ model: MODEL_ID, detail: '529: ' + MODEL_ID + ' is overloaded' }), '529: the model is overloaded');
  assert.equal(outageDetail({ detail: 'timed out' }), 'timed out');
});

const origin = (overrides = {}) => ({ organization_id: 'org_1', organization_name: 'Harbor Street Labs', scope: 'organization',
  bundle_id: 'bnd_1', revision: 4, expires_at: '2026-11-03T12:00:00Z', read_only: true, released: false, ...overrides });

test('a shared agent says where it came from, one fact per row, and is read-only', () => {
  const view = originView({ origin: origin() }, Date.parse('2026-10-04T00:00:00Z'));
  assert.equal(view.mark, 'from Harbor Street Labs');
  assert.equal(view.readOnly, true);
  assert.equal(view.released, false);
  assert.deepEqual(view.facts.map(([label]) => label), ['Shared by', 'Bundle revision', 'Shared until']);
  assert.equal(view.facts[1][1], '4');
  assert.match(view.facts[2][1], /2026/);
  assert.equal(originView({}), null);
  const expired = originView({ origin: origin({ expires_at: '2026-10-01T00:00:00Z' }) }, Date.parse('2026-10-04T00:00:00Z'));
  assert.equal(expired.facts[2][0], 'Expired');
  const repository = originView({ origin: origin({ scope: 'repository:repo_9' }) }, 0);
  assert.deepEqual(repository.facts[1], ['Shared for', 'repository repo_9']);
});

test('a released agent says no longer shared and stays read-only', () => {
  const view = originView({ origin: origin({ released: true, read_only: false }) });
  assert.equal(view.mark, 'no longer shared by Harbor Street Labs');
  assert.equal(view.readOnly, true);
  assert.deepEqual(view.facts.map(([label]) => label), ['Was shared by', 'Bundle revision']);
});

test('a collision is said on the member’s own agent', () => {
  const view = collisionView({ profile_id: 'release-notes', collision: { organization_name: 'Harbor Street Labs' } });
  assert.equal(view.mark, 'id also used by Harbor Street Labs');
  assert.match(view.banner, /will not be adopted — your agent uses the id/);
  assert.match(view.banner, /release-notes/);
  assert.equal(collisionView({ profile_id: 'x' }), null);
  assert.equal(indexRow({ profile_id: 'release-notes', collision: { organization_name: 'Harbor Street Labs' }, places: [] }).collision,
    'id also used by Harbor Street Labs');
});

test('held places, refused runs and missing routes are a place’s state and reason', () => {
  assert.match(holdWords('adoption_expired', origin({ expires_at: '2026-10-01T12:00:00Z' })), /^held — Harbor Street Labs’s bundle expired .*2026$/);
  assert.equal(holdWords('version_no_longer_shared'), 'this version is no longer shared');
  assert.equal(holdWords('adoption_ended', origin()), 'held — no longer shared by Harbor Street Labs');
  assert.equal(holdWords(''), '');
  assert.equal(refusalWords('admission_refused'), 'A rule on this device refused its model route');
  assert.deepEqual(placeStatus(managedPlace()), { state: 'on', label: 'On', reason: '' });
  assert.deepEqual(placeStatus(managedPlace({ state: 'disabled', held_reason: 'adoption_expired' })), { state: 'off', label: 'Off', reason: '' });
  assert.equal(placeStatus(managedPlace({ held_reason: 'version_no_longer_shared' })).label, 'Held');
  assert.equal(placeStatus(managedPlace({ route_problem: 'route_missing' })).label, 'Route missing');
  assert.equal(placeStatus(reviewPlace({ run_refusal: 'admission_refused' })).reason, 'A rule on this device refused its model route');
  const agent = { origin: origin(), attention: 'place_held', places: [managedPlace({ held_reason: 'version_no_longer_shared' })] };
  assert.equal(stoppedWords(agent), 'This version is no longer shared');
  assert.equal(attentionWords('place_held', agent), 'This version is no longer shared');
  assert.equal(stoppedWords({ places: [managedPlace()] }), '');
});

test('places differ by route reference, never by what a route resolves to', () => {
  assert.deepEqual(placeDifferences(managedPlace(), managedPlace({ place_id: 'y', model: 'another-model' })), []);
  assert.deepEqual(placeDifferences(managedPlace(), managedPlace({ place_id: 'y', route_id: 'rte_other' })), ['model']);
});

test('the share result shows the built path, the sign command that contains it, and the upload page', () => {
  const view = shareView({ path: '/data/bundles/organization-r5.json', signed_path: '/data/bundles/organization-r5.signed.json',
    sign_command: 'crossing-guard bundle sign --key <your organization key file> /data/bundles/organization-r5.json',
    revision: 5, published_revision: 4, expires_at: '2026-11-03T00:00:00Z',
    documents: [{ kind: 'profile', name: 'scope-watch/PROFILE.md', profile_id: 'scope-watch', change: 'changed', text_diff: [{ op: 'add', text: 'x' }] },
      { kind: 'profile', profile_id: 'lint-guard', change: 'removed' }],
    rule_changes: { added: ['a'], removed: [], changed: [{ id: 'c' }] },
    changes: [{ field: 'failure_mode', label: 'Failure mode', from: 'fail-open', to: 'fail-closed' }] }, 'https://team.example/');
  assert.equal(view.title, 'Revision 5 is built');
  assert.equal(view.against, 'Revision 4');
  assert.ok(view.signCommand.includes(view.path));
  assert.equal(view.uploadURL, 'https://team.example/policy');
  assert.deepEqual(view.documents.map(item => [item.name, item.change, item.removed]), [['scope-watch', 'changed', false], ['lint-guard', 'removed', true]]);
  assert.deepEqual(view.rules, { added: ['a'], removed: [], changed: ['c'] });
  assert.deepEqual(view.changes, [{ label: 'Failure mode', from: 'fail-open', to: 'fail-closed' }]);
  assert.equal(shareView({}).against, 'Nothing published yet');
  assert.equal(shareView({}).uploadURL, '');
});

// Criterion 56, display half: rendered with a fixture route whose model id and
// endpoint are known strings, no agent-surface view model contains either — nor
// the runtime's id or display name.
//
// The rule is about the ROUTE: which model an agent runs on is never shown on an
// agent surface. One line on an agent page keeps runtime names by owner decision
// 2026-10-04, recorded against plan §14 Q8: the delivery wording ("Messages land —
// <runtime>: <where>", deliveryWords in roster-model.js). It says where a message
// lands in a session of each runtime, not which route the agent runs on, so it is
// not among the view models swept below. The test after this one pins that
// exemption so it stays this narrow.
test('no agent-surface view model contains a model id, an endpoint or a runtime word', () => {
  const runtimeNames = { [RUNTIME_ID]: 'Fixture Runtime Name' };
  const agents = [
    { profile_id: 'helper-one', name: 'Helper one', agent_type: 'helper', attention: 'provider_outage', origin: origin(),
      places: [managedPlace(), managedPlace({ place_id: 'b', route_problem: 'route_missing', route_name: '' }),
        managedPlace({ place_id: 'c', route_id: '', route_name: '', route_problem: 'migration_failed' }),
        managedPlace({ place_id: 'd', held_reason: 'adoption_expired' })] },
    { profile_id: 'gate', name: 'Gate', agent_type: 'reviewer', places: [reviewPlace(), reviewPlace({ run_refusal: 'admission_refused' })] },
  ];
  const outage = { runtime: RUNTIME_ID, model: MODEL_ID, class: 'overloaded', detail: MODEL_ID + ' overloaded', route_names: ['Fast reader'], parked: 2, tripped_at: 1 };
  const produced = [];
  for (const agent of agents) {
    produced.push(indexRow(agent, runtimeNames), originView(agent), collisionView(agent), stoppedWords(agent), attentionWords(agent.attention, agent));
    for (const place of agent.places) {
      produced.push(routeFacts(place), fallbackFacts(place), placeStatus(place, agent.origin));
    }
  }
  produced.push(indexRows({ agents }, runtimeNames), outageWords(outage, '9:41 AM'), outageWords({ ...outage, route_names: [] }), outageDetail(outage),
    routeChoices(routes, ROUTE_FAMILY_MANAGED, () => [{ id: 'read-only', label: 'Read only' }]), routeChoices(routes, ROUTE_FAMILY_REVIEW));
  const text = JSON.stringify(produced);
  for (const forbidden of [MODEL_ID, ENDPOINT, '45999', RUNTIME_ID, 'Fixture Runtime Name']) {
    assert.equal(text.includes(forbidden), false, 'an agent-surface view model contains ' + forbidden);
  }
  assert.ok(text.includes('Fast reader') && text.includes('leaves this machine') && text.includes('on this machine'));
});

// The one exemption from the sweep above (owner decision 2026-10-04, plan §14 Q8):
// delivery wording names runtimes, because it says where a message lands. It is exempt
// for the runtime's display name only — it is built from the published delivery
// boundary and nothing of a route, so a model id, an endpoint or a runtime id on a
// capability never reaches it.
test('delivery wording is the one agent-page line that names a runtime, and names nothing of a route', () => {
  const words = deliveryWords([
    { id: RUNTIME_ID, displayName: 'Fixture Runtime Name', model: MODEL_ID, endpoint: ENDPOINT,
      messageDelivery: { supported: true, boundary: 'the next prompt' } },
    { id: 'rt-other', displayName: 'Other Runtime Name', model: MODEL_ID, messageDelivery: { supported: false, boundary: '' } },
    { id: 'rt-silent', displayName: 'Silent Runtime Name', model: MODEL_ID },
  ]);
  assert.equal(words, 'Fixture Runtime Name: the next prompt \u00b7 Other Runtime Name: not delivered');
  for (const forbidden of [MODEL_ID, ENDPOINT, '45999', RUNTIME_ID]) {
    assert.equal(words.includes(forbidden), false, 'delivery wording contains ' + forbidden);
  }
});

// Red-team Low 13: the provider's words can name a runtime and an address as well as
// the model. The banner's own line never carries them; they sit behind a disclosure
// whose closed state says nothing of them.
test('the outage banner keeps the provider’s words behind a disclosure', () => {
  const outage = { runtime: RUNTIME_ID, model: MODEL_ID, class: 'overloaded', route_names: ['Fast reader'], parked: 1,
    detail: 'Fixture Runtime Name: POST ' + ENDPOINT + ' for ' + MODEL_ID + ' returned 529' };
  const reported = outageReported(outage);
  const closed = JSON.stringify([outageWords(outage, '9:41 AM'), reported.summary]);
  for (const forbidden of [MODEL_ID, ENDPOINT, RUNTIME_ID, 'Fixture Runtime Name']) {
    assert.equal(closed.includes(forbidden), false, 'the banner shows ' + forbidden + ' before the disclosure is opened');
  }
  assert.ok(reported.text.includes('returned 529') && !reported.text.includes(MODEL_ID));
  assert.equal(outageReported({ ...outage, detail: '' }), null);
});
