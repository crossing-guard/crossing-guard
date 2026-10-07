import test from 'node:test';
import assert from 'node:assert/strict';
import {
  familyWords, routeRunsOn, migratedWords, usedBy, routeRows, routesInUse, unroutedPlaces, routeDraft, renameDraft,
  previewView, saveLabel, refusalView, FAMILY_MANAGED, FAMILY_REVIEW,
} from './settings-routes-model.js';

const names = { runtimeNames: { 'rt-a': 'Runtime A' }, modelLabels: { 'rt-a\u0000m-1': 'Model One' }, agentNames: { 'scope-watch': 'Scope watch', gate: 'Gatekeeper' } };

const fast = { route_id: 'rte_fast', name: 'Fast reader', family: FAMILY_MANAGED, local: false, locality: 'leaves this machine',
  fields: { runtime: 'rt-a', model: 'm-1', thinking_effort: { kind: 'level', value: 'low' } }, state_token: 't1',
  places: [{ place_id: 'a', lane: 'managed', profile_id: 'scope-watch', repository: 'rate-limiter', state: 'enabled' },
    { place_id: 'b', lane: 'managed', profile_id: 'scope-watch', repository: 'storefront', state: 'disabled' },
    { place_id: 'c', lane: 'managed', profile_id: 'release-notes', repository: 'storefront', state: 'enabled', fallback: true, position: 1 }] };
const careful = { route_id: 'rte_careful', name: 'Careful reviewer', family: FAMILY_REVIEW, local: true, locality: 'on this machine',
  fields: { endpoint: 'http://127.0.0.1:11434', model: 'local-model' }, migrated_at: '2026-10-04T10:00:00Z',
  places: [{ place_id: 'review', lane: 'review', profile_id: 'gate', state: 'enabled' }] };
const unused = { route_id: 'rte_unused', name: 'Another', family: FAMILY_MANAGED, local: false, fields: { runtime: 'rt-b', model: '' }, places: [] };

test('the two families are named in the owner’s words', () => {
  assert.equal(familyWords(FAMILY_MANAGED), 'Agents that watch or help in sessions');
  assert.equal(familyWords(FAMILY_REVIEW), 'The reviewer (a model on this machine)');
});

test('Settings → Models shows what a route resolves to: model id and endpoint', () => {
  // One fact per entry, each drawn on its own line: never a dotted string (Low 17, W-4).
  assert.deepEqual(routeRunsOn(fast, names.runtimeNames, names.modelLabels), ['Runtime A', 'Model One', 'effort low']);
  assert.deepEqual(routeRunsOn(fast, names.runtimeNames), ['Runtime A', 'm-1', 'effort low']);
  assert.deepEqual(routeRunsOn(unused), ['rt-b', 'default model']);
  assert.deepEqual(routeRunsOn(careful), ['local-model', 'http://127.0.0.1:11434']);
});

test('a migrated route says it came from earlier settings, with the date', () => {
  assert.match(migratedWords(careful), /^From earlier settings, .*2026$/);
  assert.equal(migratedWords(fast), '');
});

test('the places that use a route are grouped by agent, with off and fallback said', () => {
  assert.deepEqual(usedBy(fast, names.agentNames), [
    { agent: 'release-notes', places: ['storefront (fallback)'] },
    { agent: 'Scope watch', places: ['rate-limiter', 'storefront (off)'] },
  ]);
  assert.deepEqual(usedBy(careful, names.agentNames), [{ agent: 'Gatekeeper', places: ['Every repository'] }]);
  assert.deepEqual(usedBy(unused), []);
});

test('route rows sort by name and routes in use list most-used first', () => {
  const rows = routeRows({ routes: [fast, unused, careful] }, names);
  assert.deepEqual(rows.map(row => row.name), ['Another', 'Careful reviewer', 'Fast reader']);
  assert.equal(rows[2].locality, 'leaves this machine');
  assert.equal(rows[1].familyWords, 'The reviewer (a model on this machine)');
  assert.ok(rows[1].search.includes('local-model'));
  const used = routesInUse(rows);
  assert.deepEqual(used.map(item => [item.name, item.places]), [['Fast reader', '3 places'], ['Careful reviewer', 'Every repository']]);
  assert.deepEqual(used[0].agents, ['release-notes', 'Scope watch']);
  assert.deepEqual(routesInUse(routeRows({})), []);
});

test('places with no route yet and places whose route is missing are named', () => {
  const places = unroutedPlaces({ agents: [
    { profile_id: 'a', name: 'Agent A', places: [{ lane: 'managed', repository: 'docs', route_problem: 'migration_failed', runtime: 'rt-a', model: 'secret-model' },
      { lane: 'managed', repository: 'ok' }] },
    { profile_id: 'gate', name: 'Gatekeeper', places: [{ lane: 'review', route_problem: 'route_missing' }] },
  ] });
  assert.deepEqual(places.map(item => [item.agent, item.where, item.words]), [
    ['Agent A', 'docs', 'It has no route yet. It runs as it did before.'],
    ['Gatekeeper', 'Every repository', 'Its model route is missing. Asks go to you.'],
  ]);
});

test('drafts carry only the fields of their family', () => {
  assert.deepEqual(routeDraft({ name: ' Fast ', family: FAMILY_MANAGED, managed: { runtime: 'rt-a', model: 'm-1', mode: 'plan', thinking_effort: { kind: 'inherit' } } }),
    { name: 'Fast', family: FAMILY_MANAGED, fields: { runtime: 'rt-a', model: 'm-1', thinking_effort: { kind: 'inherit' } } });
  assert.deepEqual(routeDraft({ routeID: 'rte_1', name: 'Local', family: FAMILY_REVIEW, endpoint: ' http://127.0.0.1:1 ', model: ' m ' }),
    { route_id: 'rte_1', name: 'Local', family: FAMILY_REVIEW, fields: { endpoint: 'http://127.0.0.1:1', model: 'm' } });
  assert.deepEqual(renameDraft(fast, ' Quick reader '), { route_id: 'rte_fast', name: 'Quick reader', family: FAMILY_MANAGED, fields: fast.fields });
});

test('a preview names the places that follow, the problems, and the rules that apply', () => {
  const view = previewView({ route: fast, exists: true, changed: true, problems: [],
    admission: [{ allowed: true, fired: [] }, { place_id: 'a', label: 'scope-watch in rate-limiter', allowed: true, action: 'observe', rule: 'watch-routes', fired: ['watch-routes'] }] }, names);
  assert.equal(view.followCount, 3);
  assert.equal(view.canSave, true);
  assert.equal(saveLabel(view), 'Save for 3 places');
  assert.deepEqual(view.rules, [{ rule: 'watch-routes', where: ' — scope-watch in rate-limiter', refuses: false, action: 'observe' }]);
  const refused = previewView({ route: careful, exists: true, changed: true,
    problems: [{ code: 'route_edit_violates_places', message: 'This change would stop these places from running.', places: ['the reviewer (gate)'] }],
    admission: [{ allowed: false, action: 'deny', rule: 'no-remote', fired: ['no-remote'] }] }, names);
  assert.equal(refused.canSave, false);
  assert.deepEqual(refused.problems[0].places, ['the reviewer (gate)']);
  assert.equal(refused.refusedByRule, true);
  assert.equal(refused.rules[0].refuses, true);
  assert.equal(saveLabel(refused), 'Save', 'a refused edit does not say places follow');
  assert.equal(saveLabel(previewView({ route: unused, exists: false }, names)), 'Create');
  assert.equal(saveLabel(previewView({ route: unused, exists: true }, names)), 'Save');
});

test('a refusal keeps the places it names', () => {
  const error = Object.assign(new Error('This model route is still used by: a in b.'), { code: 'route_in_use', recovery: 'Choose another model route.', places: [] });
  assert.deepEqual(refusalView(error), { message: 'This model route is still used by: a in b. Choose another model route.', places: [], code: 'route_in_use' });
  assert.deepEqual(refusalView(Object.assign(new Error('x'), { places: ['p'] })).places, ['p']);
});
