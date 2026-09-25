import assert from 'node:assert/strict';
import test from 'node:test';

import { inputCapabilityIssue, reordered } from './attachments.js';

test('attachment ordering is explicit and leaves the original immutable', () => {
  const original = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];
  assert.deepEqual(reordered(original, 2, 0).map(item => item.id), ['c', 'a', 'b']);
  assert.deepEqual(original.map(item => item.id), ['a', 'b', 'c']);
  assert.deepEqual(reordered(original, -1, 2), original);
});

test('provider capability warnings never infer conditional image support', () => {
  const capability = { displayName: 'Future', inputs: [
    { kind: 'text', note: '' },
    { kind: 'image', modelConditional: true, note: 'Choose a proved vision model.' },
  ] };
  assert.equal(inputCapabilityIssue(capability, [{ kind: 'text' }]), '');
  assert.equal(inputCapabilityIssue(capability, [{ kind: 'image' }]), 'Choose a proved vision model.');
  assert.equal(inputCapabilityIssue({ displayName: 'Text only', inputs: [{ kind: 'text' }] }, [{ kind: 'image' }]),
    'Text only does not accept image attachments.');
});
