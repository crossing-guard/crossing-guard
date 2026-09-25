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
