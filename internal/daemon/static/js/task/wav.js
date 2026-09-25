// Pure audio math for dictation: resample float frames to the backend's rate,
// quantize to 16-bit little-endian PCM, and measure energy. No DOM, no fetch.

export function resample(samples, fromRate, toRate) {
  if (fromRate === toRate || samples.length === 0) return Float32Array.from(samples);
  const ratio = fromRate / toRate;
  const length = Math.max(1, Math.floor(samples.length / ratio));
  const out = new Float32Array(length);
  for (let i = 0; i < length; i++) {
    const position = i * ratio;
    const index = Math.floor(position);
    const next = Math.min(index + 1, samples.length - 1);
    const fraction = position - index;
    out[i] = samples[index] * (1 - fraction) + samples[next] * fraction;
  }
  return out;
}

export function toPCM16(samples) {
  const out = new Uint8Array(samples.length * 2);
  const view = new DataView(out.buffer);
  for (let i = 0; i < samples.length; i++) {
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    view.setInt16(i * 2, clamped < 0 ? clamped * 32768 : clamped * 32767, true);
  }
  return out;
}

// energy returns peak and RMS on a 0..1 scale over float samples, matching the
// daemon's own measure so the browser's silence stop and the daemon's floor
// agree.
export function energy(samples) {
  if (!samples.length) return { peak: 0, rms: 0 };
  let peak = 0; let sum = 0;
  for (let i = 0; i < samples.length; i++) {
    const value = Math.abs(samples[i]);
    if (value > peak) peak = value;
    sum += value * value;
  }
  return { peak, rms: Math.sqrt(sum / samples.length) };
}

export function isSilent(samples, limits) {
  const { peak, rms } = energy(samples);
  return peak < limits.min_peak || rms < limits.min_rms;
}
