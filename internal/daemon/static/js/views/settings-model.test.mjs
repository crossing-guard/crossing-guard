import test from 'node:test';
import assert from 'node:assert/strict';
import { SETTINGS_PAGES, normalizePage, pageForRequest, settingsAttention, attentionCounts,
  forgetProviderTokens, storedProviderTokens, findSettings } from './settings-model.js';

test('every page id other views use still names a page, and the old general page is the overview', () => {
  for (const id of ['agents', 'appearance', 'transcript-modes', 'views']) assert.equal(normalizePage(id), id);
  assert.equal(normalizePage('settings'), 'overview');
  assert.equal(normalizePage('nope'), '');
  assert.equal(new Set(SETTINGS_PAGES.map(([, id]) => id)).size, SETTINGS_PAGES.length);
});

test('a request for a page lands on it on every visit, and names its target', () => {
  // First visit, nothing mounted, nothing asked: the overview.
  assert.deepEqual(pageForRequest(null, null), { page: 'overview', target: '', render: true });
  // A later visit with nothing asked returns to what was open, untouched.
  assert.deepEqual(pageForRequest(null, { page: 'team', target: '' }), { page: 'team', target: '', render: false });
  // Visit, leave, ask for another page, return: the asked page is rendered.
  assert.deepEqual(pageForRequest({ page: 'agents' }, { page: 'appearance', target: '' }), { page: 'agents', target: '', render: true });
  // The same page with a different target is still a change.
  assert.equal(pageForRequest({ page: 'agents', target: 'helper-a' }, { page: 'agents', target: '' }).render, true);
  assert.equal(pageForRequest({ page: 'agents', target: 'helper-a' }, { page: 'agents', target: 'helper-a' }).render, false);
  // An unknown request keeps what is mounted.
  assert.equal(pageForRequest({ page: 'nope' }, { page: 'team', target: '' }).page, 'team');
});

const agent = (attention, name = 'Helper') => ({ profile_id: name.toLowerCase(), name, attention });

test('each roster attention code becomes a worded item that opens its agent', () => {
  for (const code of ['recent_failures', 'revision_unavailable', 'profile_problem', 'provider_outage', 'flow_ceiling_breach']) {
    const [item] = settingsAttention({ roster: { agents: [agent(code)] } });
    assert.equal(item.severity, 'bad');
    assert.deepEqual([item.page, item.target, item.title], ['agents', 'helper', 'Helper']);
    assert.ok(item.facts[0] && item.facts[0] !== code, code + ' has words');
  }
  // A code the browser has no words for is shown as itself, never as nothing.
  assert.deepEqual(settingsAttention({ roster: { agents: [agent('new_code')] } })[0].facts, ['new_code']);
  assert.deepEqual(settingsAttention({ roster: { agents: [agent('')] } }), []);
});

test('outages, stored problems and unreadable places are listed', () => {
  const items = settingsAttention({ roster: { agents: [], outages: [{ runtime: 'r', model: 'm-id', route_names: ['Fast reader'], parked: 2, detail: 'rate limit on m-id' }],
    problems: [{ problem: { message: 'A profile is broken.', recovery: 'Re-import it.' } }], places_unavailable: true } }, { r: 'Runtime R' });
  assert.equal(items.length, 3);
  // The outage is named by its model route; no model id is shown (plan §14 Q9).
  assert.match(items[0].title, /^Fast reader is not answering/);
  assert.deepEqual(items[0].facts, ['rate limit on the model']);
  assert.equal(JSON.stringify(items).includes('m-id'), false);
  assert.equal(items[1].title, 'A profile is broken.');
  assert.equal(items[2].title, 'Where agents run cannot be read');
});

test('the team items are the Team page’s needs-you block: bad for a failing link, a warning over the high-water mark', () => {
  const linked = extra => ({ state: 'linked', organization: { name: 'Acme' }, ...extra });
  for (const outcome of ['error', 'rejected', 'revoked']) {
    const [item] = settingsAttention({ team: linked({ report: { outcome, error: 'boom' } }) });
    assert.deepEqual([item.severity, item.page], ['bad', 'team'], outcome);
  }
  assert.equal(settingsAttention({ team: linked({ outbox: { outcome: 'error', error: 'internal (500)' } }) })[0].title, 'Acme’s server is not answering');
  assert.equal(settingsAttention({ team: linked({ outbox: { conflicts: { memory: 2 } } }) })[0].severity, 'warn');
  assert.equal(settingsAttention({ team: linked({ state: 'inconsistent', problem: 'the device key is missing' }) })[0].title, 'The team link is broken');
  assert.deepEqual(settingsAttention({ team: linked({ report: { outcome: 'ok' }, outbox: { outcome: 'ok' } }) }), []);
  const [warn] = settingsAttention({ team: linked({ outbox: { over_high_water: true, pending: 48721, high_water: 10000 } }) });
  assert.deepEqual([warn.severity, warn.title], ['warn', (48721).toLocaleString() + ' are waiting to send']);
  assert.deepEqual(settingsAttention({ team: { state: 'unlinked' } }), []);
});

test('a runtime carries the daemon’s code: its sentence when it has one, words otherwise', () => {
  const items = settingsAttention({ runtimeStatus: { runtimes: [
    { name: 'a', display_name: 'A', attention: 'never_fired' },
    { name: 'b', display_name: 'B', attention: 'needs_attention', problem: 'B is connected but its owned hook needs repair' },
    { name: 'c', display_name: 'C', attention: 'hook_binary_missing' },
    { name: 'd', display_name: 'D', attention: 'future_code' },
    { name: 'e', display_name: 'E' },
  ] } });
  assert.deepEqual(items.map(item => [item.severity, item.title, item.target]), [['bad', 'B', 'b'], ['bad', 'C', 'c'], ['bad', 'D', 'd'], ['info', 'A', 'a']]);
  assert.equal(items[0].facts[0], 'B is connected but its owned hook needs repair');
  assert.ok(items[1].facts[0].length > 0);
  assert.equal(items[2].facts[0], 'future_code');
});

test('dictation is listed only when the daemon says it is broken', () => {
  assert.deepEqual(settingsAttention({ speech: { state: 'unconfigured', reason: 'not set up' } }), []);
  assert.deepEqual(settingsAttention({ speech: { state: 'ready' } }), []);
  assert.equal(settingsAttention({ speech: { state: 'unavailable', reason: 'the model is missing' } })[0].facts[0], 'the model is missing');
  assert.deepEqual(settingsAttention({ speech: { state: 'ready', problems: ['checksum differs'] } })[0].facts, ['checksum differs']);
});

test('badges count what is broken or degraded, never what is only worth a look', () => {
  const items = settingsAttention({
    team: { state: 'linked', report: { outcome: 'error' }, outbox: { conflicts: { memory: 1 } } },
    roster: { agents: [agent('recent_failures', 'A'), agent('recent_failures', 'B')] },
    runtimeStatus: { runtimes: [{ name: 'x', attention: 'never_fired' }] },
  });
  assert.deepEqual(items.map(item => item.severity), ['bad', 'bad', 'bad', 'warn', 'info']);
  const counts = attentionCounts(items);
  assert.equal(counts.total, 4);
  assert.equal(counts.worst, 'bad');
  assert.deepEqual(counts.pages, { team: { count: 2, worst: 'bad' }, agents: { count: 2, worst: 'bad' } });
  assert.deepEqual(attentionCounts([{ severity: 'warn', page: 'dictation' }]), { total: 1, worst: 'warn', pages: { dictation: { count: 1, worst: 'warn' } } });
  assert.deepEqual(attentionCounts([]), { total: 0, worst: '', pages: {} });
});

test('forgetting provider tokens clears every token and address and nothing else', () => {
  const stored = { runtime: 'a', cwd: '/work', base_url: 'https://legacy', auth_token: 'legacy-token', binary: '/bin/legacy',
    runtimes: { a: { base_url: 'https://a', auth_token: 't', binary: '/bin/a', extra_args: '--x', pinned_models: ['m'] }, b: { auth_token: 'u' } } };
  assert.equal(storedProviderTokens(stored), 3);
  const kept = forgetProviderTokens(stored);
  assert.deepEqual(kept, { runtime: 'a', cwd: '/work', binary: '/bin/legacy',
    runtimes: { a: { binary: '/bin/a', extra_args: '--x', pinned_models: ['m'] }, b: {} } });
  assert.equal(storedProviderTokens(kept), 0);
  assert.equal(stored.runtimes.a.auth_token, 't', 'the stored object is not edited in place');
  assert.deepEqual(forgetProviderTokens({}), {});
});

test('find a setting matches pages and controls, and names only from what is passed in', () => {
  assert.deepEqual(findSettings('accent').map(hit => hit.page), ['appearance']);
  assert.deepEqual(findSettings('outbox')[0], { page: 'team', target: '', label: 'Waiting to send' });
  assert.deepEqual(findSettings(''), []);
  assert.deepEqual(findSettings('zzz'), []);
  assert.deepEqual(findSettings('fixture', [{ page: 'agents', target: 'fixture-helper', label: 'Fixture helper' }]),
    [{ page: 'agents', target: 'fixture-helper', label: 'Fixture helper' }]);
  // That the index itself holds no runtime or model name is the vendor lint's job:
  // settings-model.js has no baseline entry there, so one name fails the gate.
});
