import assert from 'node:assert/strict';
import {
  normalizeCapabilities, chooseChatCapability, modePairs, runtimeChatDefaults,
} from './chat-capabilities.js';

const capabilities = normalizeCapabilities([
  { runtime: 'future', display_name: 'Future', can_start: true, can_resume: true,
    interface_revision: '1.18.0', governance_lane: 'collection-only',
    governance_note: 'installed CLI 9.9.9 does not match the verified interface 1.18.0',
    modes: [{ id: '', label: 'Safe', risk: 'normal' }],
    models: [{ id: '', label: 'Default' }],
    inputs: [{ kind: 'image', media_types: ['image/png'], can_start: true, can_resume: true,
      model_conditional: true, note: 'Choose a proved vision model.' }] },
]);
assert.equal(chooseChatCapability(capabilities, 'future').runtime, 'future');
assert.deepEqual(modePairs(capabilities[0]), [['', 'Safe', '']]);
// Settings compatibility lines (Slices A + C): the normalizer must carry the
// interface revision and governance lane through — dropping them silently
// blanked the Settings rendering.
assert.equal(capabilities[0].interfaceRevision, '1.18.0');
assert.equal(capabilities[0].governanceLane, 'collection-only');
assert.match(capabilities[0].governanceNote, /does not match/);
assert.deepEqual(capabilities[0].inputs[0], {
  kind: 'image', mediaTypes: ['image/png'], canStart: true, canResume: true,
  modelConditional: true, modeConditional: false, note: 'Choose a proved vision model.',
});
assert.throws(() => normalizeCapabilities([]), /No chat runtimes/);
assert.throws(() => normalizeCapabilities([
  { runtime: 'broken', display_name: 'Broken', modes: [], models: [] },
]), /incomplete/);

const legacy = { runtime: 'first', auth_token: 'secret', binary: '/first', cwd: '/repo' };
assert.equal(runtimeChatDefaults(legacy, 'first').auth_token, 'secret');
assert.equal(runtimeChatDefaults(legacy, 'second').auth_token, undefined);
assert.equal(runtimeChatDefaults(legacy, 'second').cwd, '/repo');

console.log('chat-capabilities tests passed');

// Runtime-reported model lists (runtime-model-catalog-and-usage design §10):
// ids are opaque, unknown enum values degrade to unavailable, and the merged
// picker keeps declared entries around the runtime's list.
{
  const { normalizeChatModels, modelPairs, isCustomModelOption, findDiscoveredModel } = await import('./chat-capabilities.js');
  const alien = normalizeChatModels({ runtime: 'fakealpha', state: 'mystery', models: [
    { id: 'a|b::c', label: 'Pipe', group: 'g1', group_label: '<b>G</b>', limits: { context_tokens: 8000 },
      inputs: ['text', 'image'], price: { unit: 'credits', per_tokens: 1000, rates: [{ class: 'input', amount: 2 }] } },
    { id: 'odd ?#&"<>/ ü', label: 'Odd' }, { id: '', label: 'dropped: no id' }, { label: 'dropped: no id either' },
  ] }, 'fakealpha');
  assert.equal(alien.state, 'unavailable', 'an unknown state never reads as fresh');
  assert.deepEqual(alien.models.map(model => model.id), ['a|b::c', 'odd ?#&"<>/ ü']);
  assert.equal(findDiscoveredModel(alien, 'odd ?#&"<>/ ü').label, 'Odd', 'ids round-trip byte for byte');
  assert.equal(findDiscoveredModel(alien, 'a|b::c').contextTokens, 8000);

  const capability = { models: [{ id: '', label: 'Default' }, { id: 'custom', label: 'Exact…', custom: true }] };
  const pairs = modelPairs(capability, alien, ['odd ?#&"<>/ ü']);
  assert.deepEqual(pairs.map(pair => pair[0]), ['', 'odd ?#&"<>/ ü', 'a|b::c', 'custom'],
    'declared default, then pinned, then the rest, custom last');
  assert.ok(pairs[1][1].startsWith('★ '), 'a pinned model is marked');
  assert.ok(isCustomModelOption(capability, 'custom') && !isCustomModelOption(capability, 'a|b::c'),
    'custom comes from the declared flag, never a sentinel id');
  assert.deepEqual(modelPairs(capability).map(pair => pair[0]), ['', 'custom'], 'no list: declared entries only');
}
