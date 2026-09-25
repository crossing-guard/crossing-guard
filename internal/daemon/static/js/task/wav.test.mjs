import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resample, toPCM16, energy, isSilent } from './wav.js';

test('resample halves a 32 kHz signal to 16 kHz by linear interpolation', () => {
  const input = new Float32Array([0, 0.5, 1, 0.5, 0, -0.5, -1, -0.5]);
  const out = resample(input, 32000, 16000);
  assert.equal(out.length, 4);
  assert.deepEqual(Array.from(out), [0, 1, 0, -1]);
  assert.equal(resample(input, 16000, 16000).length, 8);
});

test('toPCM16 quantizes and clamps to signed 16-bit little endian', () => {
  const bytes = toPCM16(new Float32Array([0, 1, -1, 2]));
  const view = new DataView(bytes.buffer);
  assert.equal(view.getInt16(0, true), 0);
  assert.equal(view.getInt16(2, true), 32767);
  assert.equal(view.getInt16(4, true), -32768);
  assert.equal(view.getInt16(6, true), 32767);
});

test('energy and the silence floor agree with the daemon measure', () => {
  const silence = new Float32Array(1000);
  assert.deepEqual(energy(silence), { peak: 0, rms: 0 });
  const tone = Float32Array.from({ length: 1000 }, (_, i) => 0.5 * Math.sin(i / 7));
  const { peak, rms } = energy(tone);
  assert.ok(peak > 0.45 && peak <= 0.5 && rms > 0.3 && rms < 0.4);
  const limits = { min_peak: 0.01, min_rms: 0.003 };
  assert.equal(isSilent(silence, limits), true);
  assert.equal(isSilent(tone, limits), false);
});
