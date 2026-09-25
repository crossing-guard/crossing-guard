import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeOrchestrationError } from './orchestration-api-error.js';

const envelopeError = (payload, status = 422) => {
  const error = new Error(JSON.stringify(payload));
  error.status = status;
  return error;
};

test('typed envelope unwraps into message/code/field/recovery', () => {
  const raw = envelopeError({ error: { message: 'profile invalid', code: 'bad_profile', field: 'limits.timeout', recovery: 'Fix the timeout.' } });
  const normalized = normalizeOrchestrationError(raw, { code: 'profile_error', recovery: 'Review the profile and retry.' });
  assert.equal(normalized.message, 'profile invalid');
  assert.equal(normalized.status, 422);
  assert.equal(normalized.code, 'bad_profile');
  assert.equal(normalized.field, 'limits.timeout');
  assert.equal(normalized.recovery, 'Fix the timeout.');
});

test('missing code and recovery fall back to the lane defaults', () => {
  const raw = envelopeError({ error: { message: 'nope' } });
  const profile = normalizeOrchestrationError(raw, { code: 'profile_error', recovery: 'Review the profile and retry.' });
  assert.equal(profile.code, 'profile_error');
  assert.equal(profile.recovery, 'Review the profile and retry.');
  const review = normalizeOrchestrationError(envelopeError({ error: { message: 'nope' } }), { code: 'review_error' });
  assert.equal(review.code, 'review_error');
  assert.equal(review.recovery, '');
});

test('non-JSON message passes the original error through untouched', () => {
  const raw = new Error('HTTP 500');
  raw.status = 500;
  assert.equal(normalizeOrchestrationError(raw, { code: 'managed_error' }), raw);
});

test('JSON without the error envelope passes through untouched', () => {
  const raw = envelopeError({ note: 'fine' });
  assert.equal(normalizeOrchestrationError(raw, { code: 'agent_error' }), raw);
  const wrongShape = envelopeError({ error: { message: 42 } });
  assert.equal(normalizeOrchestrationError(wrongShape, { code: 'agent_error' }), wrongShape);
});

test('no defaults still yields the generic orchestration code', () => {
  const normalized = normalizeOrchestrationError(envelopeError({ error: { message: 'x' } }));
  assert.equal(normalized.code, 'orchestration_error');
});
